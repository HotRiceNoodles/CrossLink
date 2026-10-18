package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/pool"
)

const maxResponseRead = 50 << 20 // 50 MB cap on response body

const testTimeout = 10 * time.Second

type OpenAICompatibleProvider struct {
	name         string
	baseURL      string
	httpClient   *http.Client
	streamClient *http.Client
}

func NewOpenAICompatible(name, baseURL string, timeout time.Duration) *OpenAICompatibleProvider {
	return &OpenAICompatibleProvider{
		name:         name,
		baseURL:      strings.TrimRight(baseURL, "/"),
		httpClient:   newDefaultClient(timeout),
		streamClient: newStreamClient(),
	}
}

// NewOpenAICompatibleWithTransport is the dependency-injection seam of
// NewOpenAICompatible: it lets the caller supply the RoundTripper used for
// streaming requests (still wrapped with the standard otel/debug layers),
// while non-streaming requests keep the default SSRF-safe clients. Production
// callers use NewOpenAICompatible; tests use this to target httptest servers
// without touching global SSRF state.
func NewOpenAICompatibleWithTransport(name, baseURL string, timeout time.Duration, rt http.RoundTripper) *OpenAICompatibleProvider {
	p := NewOpenAICompatible(name, baseURL, timeout)
	if rt != nil {
		p.streamClient = &http.Client{
			CheckRedirect: guardrail.BlockInternalRedirect,
			Transport:     newWrappedTransport(rt),
		}
	}
	return p
}

func (p *OpenAICompatibleProvider) Name() string { return p.name }

func (p *OpenAICompatibleProvider) Embeddings(ctx context.Context, req *domain.EmbeddingsRequest, apiKey string) (*domain.EmbeddingsResponse, error) {
	url := p.baseURL + "/embeddings"
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	var embeddingsResp domain.EmbeddingsResponse
	if err := decodeBoundedJSON(resp, &embeddingsResp, maxResponseRead, "embeddings response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &embeddingsResp, nil
}

func (p *OpenAICompatibleProvider) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	url := p.baseURL + "/chat/completions"
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	var openaiResp domain.OpenAIResponse
	if err := decodeBoundedJSON(resp, &openaiResp, maxResponseRead, "chat response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &openaiResp, nil
}

// Responses serves the Responses API (/v1/responses) via raw upstream passthrough.
// rawBody is forwarded verbatim to <baseURL>/responses; the upstream's response
// body (JSON or SSE bytes) is returned unread so callers stream/copy it without
// field loss. Only invoked when the model's ExtraConfig sets supports_responses:true.
func (p *OpenAICompatibleProvider) Responses(ctx context.Context, rawBody []byte, apiKey string) (io.ReadCloser, int, error) {
	url := p.baseURL + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawBody))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.streamClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("send request: %w", err)
	}
	return resp.Body, resp.StatusCode, nil
}

func (p *OpenAICompatibleProvider) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	url := p.baseURL + "/chat/completions"
	reqCopy := *req
	reqCopy.Stream = true
	body, _ := json.Marshal(reqCopy)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// U-34: parse BEFORE closing — parseProviderError reads the body;
		// the old order lost the error detail and broke connection reuse.
		perr := parseProviderError(resp)
		resp.Body.Close()
		return nil, perr
	}

	ch := make(chan domain.SSEChunk, 64)
	done := make(chan struct{})
	expectedChoices := 1
	if req.N != nil && *req.N > 1 {
		expectedChoices = *req.N
	}
	// Idle watchdog: a connected-but-silent upstream must not pin this
	// goroutine and its connection forever. stopWatch fires on normal end.
	var streamBody io.Reader = resp.Body
	var stopWatch func()
	var act *atomic.Int64
	if idle := getStreamIdleTimeout(); idle > 0 {
		iw, stop := watchStreamIdle(resp.Body, idle)
		streamBody, stopWatch, act = iw, stop, iw.Activity()
	}
	go func() {
		if stopWatch != nil {
			defer stopWatch()
		}
		defer close(ch)
		defer close(done)
		defer resp.Body.Close()
		readSSEStream(ctx, streamBody, ch, expectedChoices, act)
	}()
	// Close the body when the context cancels to unblock a scanner blocked in
	// Read. Exits on done as well, so a stream that completes normally does
	// not leak this goroutine.
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		resp.Body.Close()
	}()

	return ch, nil
}

// sendSSEChunk sends one chunk, aborting when the context is cancelled (the
// consumer stopped reading). Returns false when the caller should stop.
func sendSSEChunk(ctx context.Context, ch chan<- domain.SSEChunk, chunk domain.SSEChunk) bool {
	return sendSSEChunkActivity(ctx, ch, chunk, nil)
}

// sendSSEChunkActivity is sendSSEChunk with a stream-progress timestamp.
// While BLOCKED on a full channel (slow/stalled consumer, not upstream
// silence) it retries at idle/4 and refreshes activity — the idle watchdog
// must not cut backpressured streams; the total-duration cap and client
// disconnects bound them instead. A send blocked past a full idle window is
// counted (and logged rate-limited) as POSITIVE backpressure evidence — the
// acceptance criterion is "backpressure events > 0 while watchdog cuts stay
// unchanged", not the absence of cut logs.
func sendSSEChunkActivity(ctx context.Context, ch chan<- domain.SSEChunk, chunk domain.SSEChunk, activity *atomic.Int64) bool {
	if activity == nil {
		select {
		case ch <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// Retry interval must stay well below the idle window (idle/4): a
	// heartbeat gap longer than the window reads as a stall — exactly the
	// mis-cut the backpressure regression test catches.
	retry := time.Second
	idle := getStreamIdleTimeout()
	if idle > 0 {
		if q := idle / 4; q < retry {
			retry = q
		}
	}
	if retry < 100*time.Millisecond {
		retry = 100 * time.Millisecond
	}
	blockStart := time.Now()
	overIdle := false
	for {
		select {
		case ch <- chunk:
			if overIdle {
				streamBackpressureEventsTotal.Inc()
				if now := time.Now().UnixNano(); now-lastBackpressureLogNs.Load() > int64(5*time.Second) {
					lastBackpressureLogNs.Store(now)
					slog.Info("stream consumer backpressured: send blocked past idle window, watchdog held",
						"blocked", time.Since(blockStart).Round(time.Millisecond).String(),
						"idle_timeout", idle.String())
				}
			}
			return true
		case <-ctx.Done():
			return false
		case <-time.After(retry):
			activity.Store(time.Now().UnixNano())
			if idle > 0 && !overIdle && time.Since(blockStart) > idle {
				overIdle = true // count at most once per send attempt
			}
		}
	}
}

// upstreamStreamError probes a data payload for an OpenAI-style error object.
// The probe runs on EVERY payload — some upstreams attach the error alongside
// choices — and streams carrying an error frame must be classified as
// interrupted, never as clean completion. Only the error TYPE is retained and
// only after mapping to a fixed category: the upstream type string is
// untrusted content and must never reach logs verbatim.
type upstreamStreamError struct {
	Error *struct {
		Type string `json:"type"`
	} `json:"error"`
}

// classifyUpstreamErrorType maps an untrusted upstream error type to a fixed
// log-safe category; anything outside the well-known set logs as "unknown".
func classifyUpstreamErrorType(raw string) string {
	switch raw {
	case "server_error", "overloaded_error", "rate_limit_error", "timeout",
		"authentication_error", "permission_error", "invalid_request_error",
		"not_found_error", "api_error", "billing_error", "quota_exceeded":
		return raw
	default:
		return "unknown"
	}
}

// readSSEStream relays upstream SSE lines as chunks. activity (optional,
// from the idle watchdog) is refreshed while sends block on a full channel so
// consumer backpressure is not misread as upstream silence.
func readSSEStream(ctx context.Context, body io.Reader, ch chan<- domain.SSEChunk, expectedChoices int, activity *atomic.Int64) {
	buf := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(buf)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(*buf, 4*1024*1024)

	// Some OpenAI-compatible upstreams (e.g. MiniMax) close the SSE stream
	// after the final finish_reason chunk without the `data: [DONE]` sentinel.
	// A clean EOF is only reclassified as normal completion when EVERY choice
	// the request expected (n, default 1) reported an explicit finish_reason,
	// and no cancellation, read error, malformed chunk, or upstream error
	// frame was seen; anything else stays a mid-stream interruption for the
	// caller's graceful degradation. Malformed/error frames poison the stream
	// even when a later [DONE] arrives — a sentinel must not launder a suspect
	// stream into success. Usage-tail chunks after finish_reason are still
	// forwarded: the synthesized terminal is emitted only after the stream has
	// ended.
	choiceSeen := make(map[int]bool)
	choiceFinished := make(map[int]bool)
	sawParseError := false
	sawUpstreamError := false

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if data == "[DONE]" {
				if sawParseError || sawUpstreamError {
					// A sentinel must not launder a stream that already showed
					// malformed chunks or an upstream error frame into a
					// clean completion — stay interrupted.
					slog.Warn("terminal [DONE] ignored after malformed/error frames in stream",
						"saw_parse_error", sawParseError, "saw_upstream_error", sawUpstreamError)
					return
				}
				sendSSEChunkActivity(ctx, ch, domain.SSEChunk{Done: true}, activity)
				return
			}

			var chunk domain.OpenAIChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				// Log the failure and the payload SIZE only — the raw payload
				// is untrusted content and must never reach logs.
				slog.Debug("skipped malformed SSE chunk", "error", err, "payload_bytes", len(data))
				sawParseError = true
				continue
			}
			var probe upstreamStreamError
			if json.Unmarshal([]byte(data), &probe) == nil && probe.Error != nil {
				slog.Warn("upstream error frame in stream",
					"type", classifyUpstreamErrorType(probe.Error.Type), "payload_bytes", len(data))
				sawUpstreamError = true
			}
			for _, c := range chunk.Choices {
				choiceSeen[c.Index] = true
				if c.FinishReason != nil && *c.FinishReason != "" {
					choiceFinished[c.Index] = true
				}
			}

			if !sendSSEChunkActivity(ctx, ch, domain.SSEChunk{Chunk: &chunk}, activity) {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("SSE stream read error", "error", err)
		return
	}
	if ctx.Err() != nil {
		return
	}
	if sawParseError || sawUpstreamError || expectedChoices < 1 {
		return
	}
	if len(choiceFinished) != len(choiceSeen) {
		return
	}
	// Every choice the request expected must have reported an explicit
	// finish_reason: an upstream that only served choice 0 of an n=2 request
	// did NOT complete the stream.
	for i := 0; i < expectedChoices; i++ {
		if !choiceFinished[i] {
			return
		}
	}
	slog.Info("upstream stream ended without [DONE] after all choices reported finish_reason; synthesizing terminal")
	sendSSEChunkActivity(ctx, ch, domain.SSEChunk{Done: true}, activity)
}

func (p *OpenAICompatibleProvider) GenerateImage(ctx context.Context, req *domain.ImageRequest, apiKey string) (*domain.ImageResponse, error) {
	u := p.baseURL + "/images/generations"
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.ImageResponse
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (p *OpenAICompatibleProvider) SubmitVideoTask(ctx context.Context, req *domain.VideoRequest, apiKey string) (*domain.VideoTask, error) {
	u := p.baseURL + "/videos"

	// Build OpenAI-format request body (field names: size, seconds — not aspect_ratio, duration)
	upstreamBody := map[string]any{
		"prompt": req.Prompt,
		"model":  req.Model,
	}
	if req.AspectRatio != "" {
		upstreamBody["size"] = aspectRatioToSize(req.AspectRatio)
	}
	if req.Duration > 0 {
		upstreamBody["seconds"] = strconv.Itoa(req.Duration)
	}
	body, _ := json.Marshal(upstreamBody)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.VideoTask
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	result.Status = mapOpenAIStatus(result.Status)
	return &result, nil
}

func (p *OpenAICompatibleProvider) GetVideoTaskStatus(ctx context.Context, taskID string, apiKey string) (*domain.VideoTask, error) {
	u := p.baseURL + "/videos/" + url.PathEscape(taskID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.VideoTask
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	result.Status = mapOpenAIStatus(result.Status)
	return &result, nil
}

// mapOpenAIStatus converts OpenAI status strings to internal status values.
func mapOpenAIStatus(status string) string {
	switch status {
	case "queued":
		return "pending"
	case "in_progress":
		return "processing"
	default:
		return status // "completed", "failed" are the same
	}
}

// aspectRatioToSize converts aspect ratio (e.g. "16:9") to size string (e.g. "1280x720").
func aspectRatioToSize(ratio string) string {
	switch ratio {
	case "16:9":
		return "1280x720"
	case "9:16":
		return "720x1280"
	case "1:1":
		return "1024x1024"
	default:
		return ""
	}
}

func (p *OpenAICompatibleProvider) CreateSpeech(ctx context.Context, req *domain.SpeechRequest, apiKey string) (io.ReadCloser, string, error) {
	u := p.baseURL + "/audio/speech"
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.streamClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// U-34: parse BEFORE closing (parseProviderError reads the body).
		perr := parseProviderError(resp)
		resp.Body.Close()
		return nil, "", perr
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

func (p *OpenAICompatibleProvider) Transcribe(ctx context.Context, req *domain.TranscriptionRequest, apiKey string) (*domain.TranscriptionResponse, error) {
	return p.transcribeOrTranslate(ctx, req, apiKey, "/audio/transcriptions")
}

func (p *OpenAICompatibleProvider) Translate(ctx context.Context, req *domain.TranscriptionRequest, apiKey string) (*domain.TranscriptionResponse, error) {
	return p.transcribeOrTranslate(ctx, req, apiKey, "/audio/translations")
}

func (p *OpenAICompatibleProvider) transcribeOrTranslate(ctx context.Context, req *domain.TranscriptionRequest, apiKey string, path string) (*domain.TranscriptionResponse, error) {
	u := p.baseURL + path
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", req.FileName)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(req.FileData); err != nil {
		return nil, fmt.Errorf("write file data: %w", err)
	}
	if err := writer.WriteField("model", req.Model); err != nil {
		return nil, fmt.Errorf("write model field: %w", err)
	}
	if req.Language != "" {
		writer.WriteField("language", req.Language)
	}
	if req.Prompt != "" {
		writer.WriteField("prompt", req.Prompt)
	}
	if req.ResponseFormat != "" {
		writer.WriteField("response_format", req.ResponseFormat)
	}
	if req.Temperature > 0 {
		writer.WriteField("temperature", strconv.FormatFloat(req.Temperature, 'f', -1, 64))
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	// Handle non-JSON response formats (text, srt, vtt)
	if req.ResponseFormat != "" && req.ResponseFormat != "json" && req.ResponseFormat != "verbose_json" {
		// Text formats: same bounded-total-length contract — read max+1 and
		// reject over-cap instead of silently truncating the transcript.
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseRead+1))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if int64(len(data)) > maxResponseRead {
			return nil, fmt.Errorf("transcription response exceeds %d byte cap", maxResponseRead)
		}
		return &domain.TranscriptionResponse{Text: string(data)}, nil
	}
	var result domain.TranscriptionResponse
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (p *OpenAICompatibleProvider) CreateBatch(ctx context.Context, req *domain.BatchRequest, apiKey string) (*domain.BatchResponse, error) {
	u := p.baseURL + "/batch"
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.BatchResponse
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (p *OpenAICompatibleProvider) GetBatch(ctx context.Context, batchID string, apiKey string) (*domain.BatchResponse, error) {
	u := p.baseURL + "/batch/" + batchID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.BatchResponse
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (p *OpenAICompatibleProvider) ListBatches(ctx context.Context, params url.Values, apiKey string) (*domain.BatchListResponse, error) {
	u := p.baseURL + "/batches"
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.BatchListResponse
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// GetFileContent downloads a Files API file (e.g. a batch output/error file)
// so callers can extract per-request usage. The caller must Close the stream.
func (p *OpenAICompatibleProvider) GetFileContent(ctx context.Context, fileID, apiKey string) (io.ReadCloser, error) {
	u := p.baseURL + "/files/" + fileID + "/content"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// U-34: parse BEFORE closing (parseProviderError reads the body).
		perr := parseProviderError(resp)
		resp.Body.Close()
		return nil, perr
	}
	return resp.Body, nil
}

func (p *OpenAICompatibleProvider) CancelBatch(ctx context.Context, batchID string, apiKey string) (*domain.BatchResponse, error) {
	u := p.baseURL + "/batch/" + batchID + "/cancel"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}
	var result domain.BatchResponse
	if err := decodeBoundedJSON(resp, &result, maxResponseRead, "provider response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func init() {
	RegisterAdapter("openai_compatible", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewOpenAICompatible(p.Name, p.BaseURL, timeout), nil
	}, &AdapterMeta{
		DisplayName:    "OpenAI Compatible",
		Description:    "OpenAI API compatible provider (DeepSeek, Qwen, Moonshot, etc.)",
		AdapterVersion: "1",
		NeedsBaseURL:   true,
		NeedsAPIKey:    true,
		Capabilities:   []string{"chat", "stream", "embeddings", "images", "video", "audio_speech", "audio_transcription", "audio_translation", "batch"},
		ExtraFields: []AdapterField{
			{Name: "record", Label: "录制响应（Mock 回放）", Type: "switch", DefaultValue: "false"},
		},
	})

	RegisterAdapter("ollama", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewOpenAICompatible(p.Name, p.BaseURL, timeout), nil
	}, &AdapterMeta{
		DisplayName:    "Ollama",
		Description:    "Local LLM inference server",
		AdapterVersion: "1",
		NeedsBaseURL:   true,
		NeedsAPIKey:    false,
		Capabilities:   []string{"chat", "stream"},
		ExtraFields: []AdapterField{
			{Name: "record", Label: "录制响应（Mock 回放）", Type: "switch", DefaultValue: "false"},
		},
	})
}
