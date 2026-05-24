package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(v int) *int { return &v }

func TestAnthropicProvider_Chat_Success(t *testing.T) {
	anthropicResp := map[string]any{
		"id":    "msg_123",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-sonnet-4-20250514",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello!"},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/v1/messages", r.URL.Path)
		assert.Equal(t, "test-key", r.Header.Get("x-api-key"))
		assert.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResp)
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("anthropic", server.URL, "test-key", nil, 10*time.Second)
	require.NoError(t, err)

	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(1024),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}

	got, err := p.Chat(context.Background(), req, "test-key")
	require.NoError(t, err)
	assert.Equal(t, "msg_123", got.ID)
	assert.Equal(t, "Hello!", got.Choices[0].Message.Content)
	assert.Equal(t, "stop", got.Choices[0].FinishReason)
	assert.Equal(t, 10, got.Usage.PromptTokens)
	assert.Equal(t, 5, got.Usage.CompletionTokens)
}

func TestAnthropicProvider_Chat_ProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "rate limit exceeded"},
		})
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("anthropic", server.URL, "test-key", nil, 10*time.Second)
	require.NoError(t, err)

	_, err = p.Chat(context.Background(), &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}, "test-key")
	assert.Error(t, err)
}

func TestAnthropicProvider_Chat_UsesStoredKey(t *testing.T) {
	anthropicResp := map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": "ok"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "stored-key", r.Header.Get("x-api-key"))
		json.NewEncoder(w).Encode(anthropicResp)
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("anthropic", server.URL, "stored-key", nil, 10*time.Second)
	require.NoError(t, err)

	_, err = p.Chat(context.Background(), &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}, "") // empty apiKey → should use stored key
	require.NoError(t, err)
}

func TestAnthropicProvider_StreamChat_Success(t *testing.T) {
	sseData := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/messages", r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseData))
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("anthropic", server.URL, "test-key", nil, 10*time.Second)
	require.NoError(t, err)

	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
		Stream:    true,
	}

	ch, err := p.StreamChat(context.Background(), req, "test-key")
	require.NoError(t, err)

	var chunks []domain.SSEChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// message_start(role chunk) + text delta "Hello" + text delta "!" + message_delta(finish) + message_stop(done)
	assert.GreaterOrEqual(t, len(chunks), 4)

	// First chunk should have role
	assert.Equal(t, "assistant", chunks[0].Chunk.Choices[0].Delta.Role)

	// Find text chunks
	var textContent string
	for _, c := range chunks {
		if c.Chunk != nil && c.Chunk.Choices[0].Delta.Content != "" {
			textContent += c.Chunk.Choices[0].Delta.Content
		}
	}
	assert.Equal(t, "Hello!", textContent)

	// Last chunk should be Done
	assert.True(t, chunks[len(chunks)-1].Done)
}

func TestAnthropicProvider_StreamChat_ProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("anthropic", server.URL, "test-key", nil, 10*time.Second)
	require.NoError(t, err)

	_, err = p.StreamChat(context.Background(), &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}, "test-key")
	assert.Error(t, err)
}

func TestAnthropicProvider_ExtraConfig_APIVersion(t *testing.T) {
	extraConfig, _ := json.Marshal(map[string]string{"api_version": "2024-01-01"})

	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("anthropic", server.URL, "key", extraConfig, 10*time.Second)
	require.NoError(t, err)

	_, err = p.Chat(context.Background(), &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}, "key")
	require.NoError(t, err)
	assert.Equal(t, "2024-01-01", captured)
}
