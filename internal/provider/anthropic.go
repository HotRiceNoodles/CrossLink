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
	"time"

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

type AnthropicProvider struct {
	name         string
	baseURL      string
	apiKey       string
	apiVersion   string
	httpClient   *http.Client
	streamClient *http.Client
}

type anthropicExtraConfig struct {
	APIVersion string `json:"api_version"`
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
		if err := json.Unmarshal(extraConfig, &cfg); err == nil && cfg.APIVersion != "" {
			p.apiVersion = cfg.APIVersion
		}
	}

	return p, nil
}

func (p *AnthropicProvider) Name() string { return p.name }

func (p *AnthropicProvider) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	anthropicReq, err := translator.OpenAIToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnthropicResponseSize)).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return translator.AnthropicToOpenAIResponse(&anthropicResp)
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	anthropicReq, err := translator.OpenAIToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("translate request: %w", err)
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
		resp.Body.Close()
		return nil, parseProviderError(resp)
	}

	ch := make(chan domain.SSEChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		p.readAnthropicSSE(resp.Body, ch)
	}()

	return ch, nil
}

func (p *AnthropicProvider) setHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", p.apiVersion)
}

func (p *AnthropicProvider) readAnthropicSSE(body io.Reader, ch chan<- domain.SSEChunk) {
	buf := pool.GetScannerBuffer()
	defer pool.PutScannerBuffer(buf)

	t := translator.NewReverseStreamTranslator()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(*buf, 4*1024*1024)

	var eventType string

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
			chunks := t.TranslateEvent(eventType, data)
			for _, chunk := range chunks {
				ch <- chunk
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
		DisplayName:  "Anthropic",
		Description:  "Anthropic Claude API (native)",
		NeedsBaseURL: true,
		NeedsAPIKey:  true,
		Capabilities: []string{"chat", "stream"},
		ExtraFields: []AdapterField{
			{Name: "api_version", Type: "text", Label: "API Version", Placeholder: "2023-06-01", DefaultValue: "2023-06-01"},
			{Name: "record", Label: "录制响应（Mock 回放）", Type: "switch", DefaultValue: "false"},
		},
	})
}
