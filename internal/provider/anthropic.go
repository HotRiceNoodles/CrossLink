package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/pool"
	"github.com/crosslink/internal/translator"
)

// maxAnthropicResponseSize caps the bytes read when decoding a non-streaming
// Anthropic response, preventing a malicious/buggy upstream from exhausting
// memory with an arbitrarily large JSON body (mirrors the OpenAI path's
// maxResponseRead). 50MB matches the rest of the provider layer.
const maxAnthropicResponseSize = 50 << 20

// supportedAnthropicAPIVersions is the single registry of API versions this
// adapter has verified behavior for (M0). Unknown versions are rejected at
// config validation (pre-persist) and again at construction — never inferred
// from a date comparison.
var supportedAnthropicAPIVersions = map[string]bool{
	"2023-06-01": true,
	"2026-10-21": true,
}

// anthropicAPIVersionsWithSamplingRules lists versions whose upstream rejects
// temperature/top_p/top_k outright (the 2026-10-21 breaking change). Explicit
// set, not a >= comparison, so future versions opt in only after verification.
var anthropicAPIVersionsWithSamplingRules = map[string]bool{
	"2026-10-21": true,
}

// anthropicVersionCapabilities formalizes per-version behavior (M2): the
// sampling defaults eligible for silent strip are REGISTERED here rather than
// scattered as literals in the checkpoint — a new version opts in by adding an
// entry alongside supportedAnthropicAPIVersions.
type anthropicVersionCapabilities struct {
	// SamplingDefaults maps a sampling parameter to its officially documented
	// default value. A present parameter equal to its registered default
	// strips losslessly (the upstream only accepts defaults anyway); a
	// parameter with NO registered default is always the non-default tier.
	SamplingDefaults map[string]float64
}

var anthropicVersionCaps = map[string]anthropicVersionCapabilities{
	"2026-10-21": {SamplingDefaults: map[string]float64{"temperature": 1.0}},
}

type AnthropicProvider struct {
	name                string
	baseURL             string
	apiKey              string
	apiVersion          string
	samplingRules       bool // apiVersion is in anthropicAPIVersionsWithSamplingRules
	samplingCompatStrip bool // extra_config.sampling_compat_strip
	httpClient          *http.Client
	streamClient        *http.Client
}

type anthropicExtraConfig struct {
	APIVersion          string `json:"api_version"`
	SamplingCompatStrip bool   `json:"sampling_compat_strip"`
}

// ValidateAnthropicAPIVersion checks the api_version field of an extra_config
// JSON against the supported list. It applies to any provider config carrying
// api_version (the anthropic adapter and the cloud shells that reuse it via
// api_protocol=anthropic) and is meant to run BEFORE persisting provider rows
// (admin CRUD, onboarding, import/seed), so an invalid value fails the write
// instead of surfacing at request time or being skipped at startup.
func ValidateAnthropicAPIVersion(extraConfig []byte) error {
	if len(extraConfig) == 0 {
		return nil
	}
	var cfg anthropicExtraConfig
	if err := json.Unmarshal(extraConfig, &cfg); err != nil || cfg.APIVersion == "" {
		return nil
	}
	if !supportedAnthropicAPIVersions[cfg.APIVersion] {
		return fmt.Errorf("unsupported anthropic api_version %q (supported: 2023-06-01, 2026-10-21)", cfg.APIVersion)
	}
	return nil
}

func NewAnthropicProvider(name, baseURL, apiKey string, extraConfig []byte, timeout time.Duration) (*AnthropicProvider, error) {
	p := &AnthropicProvider{
		name:         name,
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		apiVersion:   "2023-06-01",
		httpClient:   newDefaultClient(timeout),
		streamClient: newStreamClient(),
	}

	if len(extraConfig) > 0 {
		var cfg anthropicExtraConfig
		if err := json.Unmarshal(extraConfig, &cfg); err == nil {
			if cfg.APIVersion != "" {
				if !supportedAnthropicAPIVersions[cfg.APIVersion] {
					return nil, fmt.Errorf("unsupported anthropic api_version %q (supported: 2023-06-01, 2026-10-21)", cfg.APIVersion)
				}
				p.apiVersion = cfg.APIVersion
			}
			p.samplingCompatStrip = cfg.SamplingCompatStrip
		}
	}
	p.samplingRules = anthropicAPIVersionsWithSamplingRules[p.apiVersion]

	return p, nil
}

func (p *AnthropicProvider) Name() string { return p.name }

// ListUpstreamModels enumerates models from Anthropic's GET /v1/models endpoint.
// 该适配器此前未实现 ModelsLister，接入向导的 probe 对 Anthropic 一律降级为
// 1-token 聊天探测（models_supported=false，无法列出模型目录）——2026-11-10 补齐，
// probe 侧按接口断言自动生效（onboarding.go Probe → ModelsLister）。
// 与 Chat 同一 base_url 约定（不含 /v1，路径内拼接）；单页 limit=1000（API 上限，
// 实际可见模型数远低于此，未实现 has_more 游标翻页）。
func (p *AnthropicProvider) ListUpstreamModels(ctx context.Context, apiKey string) ([]UpstreamModel, error) {
	key := apiKey
	if key == "" {
		key = p.apiKey
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/models?limit=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	p.setHeaders(httpReq, key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	// Anthropic 信封：{"data":[{"id":"claude-...","display_name":"...","type":"model",...}]}
	// type 原样映射 OwnedBy（上游真实字段，不编造归属）。
	var envelope struct {
		Data []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := decodeBoundedJSON(resp, &envelope, maxAnthropicResponseSize, "anthropic models response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	out := make([]UpstreamModel, 0, len(envelope.Data))
	for _, m := range envelope.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, UpstreamModel{ID: m.ID, OwnedBy: m.Type})
	}
	return out, nil
}

func (p *AnthropicProvider) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	anthropicReq, err := translator.OpenAIToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
	}
	if err := p.applyVersionSamplingRules(ctx, anthropicReq, req.InboundSamplingMeta); err != nil {
		return nil, err
	}
	anthropicReq.Stream = false

	key := apiKey
	if key == "" {
		key = p.apiKey
	}

	body, _ := json.Marshal(anthropicReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	p.setHeaders(httpReq, key)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, parseProviderError(resp)
	}

	var anthropicResp domain.AnthropicResponse
	if err := decodeBoundedJSON(resp, &anthropicResp, maxAnthropicResponseSize, "anthropic response"); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return translator.AnthropicToOpenAIResponse(&anthropicResp)
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	anthropicReq, err := translator.OpenAIToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
	}
	if err := p.applyVersionSamplingRules(ctx, anthropicReq, req.InboundSamplingMeta); err != nil {
		return nil, err
	}
	anthropicReq.Stream = true

	key := apiKey
	if key == "" {
		key = p.apiKey
	}

	body, _ := json.Marshal(anthropicReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	p.setHeaders(httpReq, key)

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
		p.readAnthropicSSE(ctx, streamBody, ch, act)
	}()
	// Close the body when the context cancels to unblock a scanner blocked in
	// Read; exits on done so normal completion does not leak the goroutine.
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		resp.Body.Close()
	}()

	return ch, nil
}

func (p *AnthropicProvider) setHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", p.apiVersion)
}

// applyVersionSamplingRules enforces the 2026-10-21 sampling-parameter removal
// at the outbound serialization checkpoint (M0, three tiers):
//   - temperature=1.0 (the officially documented default): stripped silently,
//     audited — official semantics say new models only accept defaults, so
//     stripping a default value is lossless;
//   - non-default values: rejected with a client-facing 400-class error unless
//     the provider sets extra_config.sampling_compat_strip (then stripped and
//     audited);
//   - top_p/top_k: no published defaults are registered in the capability
//     table, so they are always the non-default tier.
//
// top_k reaches this checkpoint via req.InboundSamplingMeta — the internal
// OpenAI representation cannot express it and AnthropicToOpenAI would
// otherwise drop it before the check. Old versions are a strict no-op:
// existing translation behavior is unchanged, including the pre-existing
// top_k drop for 2023-06-01 upstreams.
//
// Every strip is recorded to the context's audit sink (upstream.ParamStrip →
// usage-log attempts timeline); rejections surface via the typed error.
func (p *AnthropicProvider) applyVersionSamplingRules(ctx context.Context, ar *domain.AnthropicRequest, meta *domain.InboundSamplingMeta) error {
	if !p.samplingRules {
		return nil
	}
	var stripped []string
	record := func(param, reason string) {
		stripped = append(stripped, param+":"+reason)
		upstream.RecordParamStrip(ctx, upstream.ParamStrip{
			Provider: p.name, APIVersion: p.apiVersion, Param: param, Reason: reason,
		})
	}
	reject := func(param string) error {
		return &ProviderError{
			StatusCode: http.StatusBadRequest,
			ErrorType:  ErrorBadRequest,
			Param:      param,
			Message: fmt.Sprintf("sampling parameter %q is not supported by Anthropic API version %s on provider %q; remove it or enable sampling_compat_strip on the provider",
				param, p.apiVersion, p.name),
		}
	}
	caps := anthropicVersionCaps[p.apiVersion]
	if ar.Temperature != nil {
		if def, hasDefault := caps.SamplingDefaults["temperature"]; hasDefault && *ar.Temperature == def {
			record("temperature", "default")
			ar.Temperature = nil
		} else if p.samplingCompatStrip {
			record("temperature", "compat_strip")
			ar.Temperature = nil
		} else {
			return reject("temperature")
		}
	}
	if ar.TopP != nil {
		if p.samplingCompatStrip {
			record("top_p", "compat_strip")
			ar.TopP = nil
		} else {
			return reject("top_p")
		}
	}
	if ar.TopK != nil || (meta != nil && meta.TopK != nil) {
		if p.samplingCompatStrip {
			record("top_k", "compat_strip")
			ar.TopK = nil
		} else {
			return reject("top_k")
		}
	}
	if len(stripped) > 0 {
		// Structured log line mirrors the audit sink for operators tailing
		// logs; the usage-log attempts row is the durable audit record.
		slog.Warn("anthropic sampling parameters stripped for api_version",
			"provider", p.name, "api_version", p.apiVersion, "params", strings.Join(stripped, ","))
	}
	return nil
}

// activity refreshes while sends block on a full channel (consumer
// backpressure) so the idle watchdog only cuts genuinely silent upstreams.
func (p *AnthropicProvider) readAnthropicSSE(ctx context.Context, body io.Reader, ch chan<- domain.SSEChunk, activity *atomic.Int64) {
	buf := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(buf)

	t := translator.NewReverseStreamTranslator()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(*buf, 4*1024*1024)

	var eventType string
	// An upstream `event: error` frame poisons the stream: the terminal
	// message_stop must not launder it into a clean completion — the same rule
	// readSSEStream applies to a post-error [DONE] sentinel.
	sawUpstreamError := false

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := []byte(strings.TrimPrefix(line, "data: "))

			if eventType == "error" {
				// Log only the mapped type and payload size — the frame is
				// untrusted content and must not reach logs verbatim.
				typ := "unknown"
				var probe struct {
					Error *struct {
						Type string `json:"type"`
					} `json:"error"`
				}
				if json.Unmarshal(data, &probe) == nil && probe.Error != nil {
					typ = classifyUpstreamErrorType(probe.Error.Type)
				}
				slog.Warn("anthropic upstream error frame in stream", "type", typ, "payload_bytes", len(data))
				sawUpstreamError = true
				eventType = ""
				continue
			}
			if eventType == "message_stop" && sawUpstreamError {
				eventType = ""
				continue
			}

			chunks := t.TranslateEvent(eventType, data)
			for _, chunk := range chunks {
				if !sendSSEChunkActivity(ctx, ch, chunk, activity) {
					return
				}
			}
			eventType = ""
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("SSE stream read error", "error", err)
	}
}

func init() {
	RegisterAdapter("anthropic", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewAnthropicProvider(p.Name, p.BaseURL, p.APIKey, p.ExtraConfig, timeout)
	}, &AdapterMeta{
		DisplayName:    "Anthropic",
		Description:    "Anthropic Claude API (native)",
		AdapterVersion: "1",
		NeedsBaseURL:   true,
		NeedsAPIKey:    true,
		Capabilities:   []string{"chat", "stream"},
		ExtraFields: []AdapterField{
			{
				Name: "api_version", Type: "select", Label: "API Version", DefaultValue: "2023-06-01",
				Options: []FieldOption{
					{Label: "2023-06-01（旧版）", Value: "2023-06-01"},
					{Label: "2026-10-21（sampling 参数已移除）", Value: "2026-10-21"},
				},
			},
			{Name: "http_timeout_seconds", Type: "number", Label: "HTTP 超时（秒，非流式，5–600）", Placeholder: "300", DefaultValue: "300"},
			{Name: "record", Label: "录制响应（Mock 回放）", Type: "switch", DefaultValue: "false"},
		},
	})
}
