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
	"time"

	"github.com/crosslink/internal/domain"
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&embeddingsResp); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&openaiResp); err != nil {
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
		resp.Body.Close()
		return nil, parseProviderError(resp)
	}

	ch := make(chan domain.SSEChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		// Close body when context cancels to unblock scanner
		go func() {
			<-ctx.Done()
			resp.Body.Close()
		}()
		readSSEStream(resp.Body, ch)
	}()

	return ch, nil
}

func readSSEStream(body io.Reader, ch chan<- domain.SSEChunk) {
	buf := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(buf)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(*buf, 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if data == "[DONE]" {
				ch <- domain.SSEChunk{Done: true}
				return
			}

			var chunk domain.OpenAIChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				preview := data
				if len(preview) > 200 {
					preview = preview[:200]
				}
				slog.Debug("skipped malformed SSE chunk", "error", err, "data", preview)
				continue
			}

			ch <- domain.SSEChunk{Chunk: &chunk}
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("SSE stream read error", "error", err)
	}
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
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
		resp.Body.Close()
		return nil, "", parseProviderError(resp)
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
		data, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		return &domain.TranscriptionResponse{Text: string(data)}, nil
	}
	var result domain.TranscriptionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func init() {
	RegisterAdapter("openai_compatible", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewOpenAICompatible(p.Name, p.BaseURL, timeout), nil
	}, &AdapterMeta{
		DisplayName:  "OpenAI Compatible",
		Description:  "OpenAI API compatible provider (DeepSeek, Qwen, Moonshot, etc.)",
		NeedsBaseURL: true,
		NeedsAPIKey:  true,
		Capabilities: []string{"chat", "stream", "embeddings", "images", "video", "audio_speech", "audio_transcription", "audio_translation", "batch"},
		ExtraFields:  []AdapterField{},
	})

	RegisterAdapter("ollama", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewOpenAICompatible(p.Name, p.BaseURL, timeout), nil
	}, &AdapterMeta{
		DisplayName:  "Ollama",
		Description:  "Local LLM inference server",
		NeedsBaseURL: true,
		NeedsAPIKey:  false,
		Capabilities: []string{"chat", "stream"},
		ExtraFields:  []AdapterField{},
	})
}
