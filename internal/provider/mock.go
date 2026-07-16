package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/pkg/token"
	"gorm.io/datatypes"
)

// MockProvider is a no-op Provider that returns canned responses without calling
// any upstream API. Used for development, CI/CD, and demos where real API keys
// or quota are unavailable. It implements the standard Provider interface, so
// the full gateway chain (auth → routing → guardrails → context assembler →
// cache → usage tracking) works unchanged — only the "upstream" is mocked.
//
// See docs/plans/2026-07-15-mock-adapter-design.md.
type MockProvider struct {
	name         string
	responseText string       // custom response text (empty = fixed mode)
	delayMs      int          // simulated latency (0 = instant)
	fixtures     FixtureStore // optional: VCR playback (nil = canned only)
}

func (p *MockProvider) Name() string { return p.name }

// activeFixtureStore returns the fixture store to use for VCR playback.
// Reads the package-level globalFixtureStore dynamically (not cached at
// construction time) because MockProvider instances are constructed during
// ProvideInfrastructure → RegisterProvidersFromDB, BEFORE SetGlobalFixtureStore
// runs in FullSetup. Caching at construction would capture nil.
func (p *MockProvider) activeFixtureStore() FixtureStore {
	if p.fixtures != nil {
		return p.fixtures
	}
	return globalFixtureStore
}

// Chat returns a recorded response (if fixture exists) or a canned response.
func (p *MockProvider) Chat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (*domain.OpenAIResponse, error) {
	// VCR playback: check fixture first.
	store := p.activeFixtureStore()
	if store != nil {
		if f, ok, _ := store.Lookup(ctx, req.Model, RequestHash(req)); ok && !f.IsStream {
			var resp domain.OpenAIResponse
			if json.Unmarshal(f.ResponseBody, &resp) == nil {
				// Apply configured delay (consistent with StreamChat VCR path).
				if p.delayMs > 0 {
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(time.Duration(p.delayMs) * time.Millisecond):
					}
				}
				return &resp, nil
			}
		}
	}
	// Fallback: canned response.
	if p.delayMs > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(p.delayMs) * time.Millisecond):
		}
	}

	text := p.buildResponse(req)
	promptTokens := estimateMessages(req.Messages)
	completionTokens := tokenEstimate(text)

	return &domain.OpenAIResponse{
		ID:      mockID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []domain.OpenAIChoice{{
			Index:        0,
			Message:      domain.OpenAIMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
		Usage: domain.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}, nil
}

// StreamChat returns a channel that emits recorded SSE chunks (if fixture exists)
// or canned chunks. Checks ctx.Done() between chunks to avoid goroutine leaks.
func (p *MockProvider) StreamChat(ctx context.Context, req *domain.OpenAIRequest, apiKey string) (<-chan domain.SSEChunk, error) {
	// VCR playback: check stream fixture first.
	store := p.activeFixtureStore()
	if store != nil {
		if f, ok, _ := store.Lookup(ctx, req.Model, RequestHash(req)); ok && f.IsStream {
			var chunks []domain.SSEChunk
			if json.Unmarshal(f.StreamChunks, &chunks) == nil && len(chunks) > 0 {
				ch := make(chan domain.SSEChunk, len(chunks))
				go func() {
					defer close(ch)
					for _, c := range chunks {
						if !sendChunk(ctx, ch, c, p.delayMs) {
							return
						}
					}
				}()
				return ch, nil
			}
		}
	}
	// Fallback: canned stream.
	text := p.buildResponse(req)
	parts := splitText(text, 4)
	completionTokens := tokenEstimate(text)

	ch := make(chan domain.SSEChunk, 8)
	go func() {
		defer close(ch)
		// First chunk carries role=assistant.
		if !sendChunk(ctx, ch, chunkWithRole("assistant", parts[0]), p.delayMs) {
			return
		}
		// Middle chunks carry content only.
		for i := 1; i < len(parts); i++ {
			if !sendChunk(ctx, ch, chunkWithContent(parts[i]), p.delayMs) {
				return
			}
		}
		// Last chunk carries finish_reason + usage.
		sendChunk(ctx, ch, chunkWithFinish(completionTokens), 0)
		// [DONE]
		sendChunk(ctx, ch, domain.SSEChunk{Done: true}, 0)
	}()
	return ch, nil
}

// buildResponse returns the mock response text. Fixed mode does NOT echo user
// content (PII safety) — only model name + message count.
func (p *MockProvider) buildResponse(req *domain.OpenAIRequest) string {
	if p.responseText != "" {
		return p.responseText
	}
	return fmt.Sprintf("[Mock] Model: %s. Received %d messages. This is a simulated response.", req.Model, len(req.Messages))
}

// --- helpers ---

func mockID() string {
	return fmt.Sprintf("mock-%d", time.Now().UnixNano())
}

// parseMockConfig reads response_text + delay_ms from ExtraConfig JSON.
// Returns ("", 0) for nil/empty/invalid input.
func parseMockConfig(raw datatypes.JSON) (responseText string, delayMs int) {
	if len(raw) == 0 {
		return "", 0
	}
	var cfg struct {
		ResponseText string `json:"response_text"`
		DelayMs      int    `json:"delay_ms"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return "", 0
	}
	return cfg.ResponseText, cfg.DelayMs
}

// splitText divides text into at most n parts by rune (UTF-8 safe). Short text
// (≤ n runes) returns a single element.
func splitText(text string, n int) []string {
	runes := []rune(text)
	if len(runes) <= n {
		return []string{text}
	}
	partLen := len(runes) / n
	var parts []string
	for i := 0; i < len(runes); i += partLen {
		end := i + partLen
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[i:end]))
	}
	return parts
}

func tokenEstimate(text string) int {
	return token.Estimate(text)
}

func estimateMessages(messages []domain.OpenAIMessage) int {
	total := 0
	for _, m := range messages {
		total += tokenEstimate(domain.ContentText(m.Content))
	}
	return total
}

// sendChunk sends one SSE chunk, respecting delay and ctx cancellation.
// Returns false if ctx was cancelled (caller should stop).
func sendChunk(ctx context.Context, ch chan<- domain.SSEChunk, chunk domain.SSEChunk, delayMs int) bool {
	if delayMs > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(delayMs) * time.Millisecond):
		}
	}
	select {
	case <-ctx.Done():
		return false
	case ch <- chunk:
		return true
	}
}

func chunkWithRole(role, content string) domain.SSEChunk {
	return domain.SSEChunk{Chunk: &domain.OpenAIChunk{
		ID:      mockID(),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Choices: []domain.OpenAIChunkChoice{{
			Index: 0,
			Delta: domain.OpenAIChunkDelta{Role: role, Content: content},
		}},
	}}
}

func chunkWithContent(content string) domain.SSEChunk {
	return domain.SSEChunk{Chunk: &domain.OpenAIChunk{
		ID:      mockID(),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Choices: []domain.OpenAIChunkChoice{{
			Index: 0,
			Delta: domain.OpenAIChunkDelta{Content: content},
		}},
	}}
}

func chunkWithFinish(completionTokens int) domain.SSEChunk {
	reason := "stop"
	return domain.SSEChunk{Chunk: &domain.OpenAIChunk{
		ID:      mockID(),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Choices: []domain.OpenAIChunkChoice{{
			Index:        0,
			Delta:        domain.OpenAIChunkDelta{},
			FinishReason: &reason,
		}},
		Usage: &domain.OpenAIChunkUsage{
			CompletionTokens: completionTokens,
		},
	}}
}

func init() {
	RegisterAdapter("mock", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		rt, delay := parseMockConfig(p.ExtraConfig)
		return &MockProvider{name: p.Name, responseText: rt, delayMs: delay, fixtures: globalFixtureStore}, nil
	}, &AdapterMeta{
		DisplayName:  "Mock (测试用)",
		Description:  "返回伪造响应，不调用上游。用于开发/CI/Demo。",
		NeedsBaseURL: false,
		NeedsAPIKey:  false,
		Capabilities: []string{"chat", "stream"},
		ExtraFields: []AdapterField{
			{Name: "response_text", Label: "自定义响应文本", Type: "textarea",
				Placeholder: "留空=固定模式"},
			{Name: "delay_ms", Label: "模拟延迟(ms)", Type: "number", DefaultValue: "0"},
		},
	})
}
