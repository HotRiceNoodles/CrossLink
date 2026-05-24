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

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	mock := &mockProvider{name: "deepseek"}
	r.Register("deepseek", mock)

	p, ok := r.Get("deepseek")
	assert.True(t, ok)
	assert.Equal(t, "deepseek", p.Name())

	_, ok = r.Get("unknown")
	assert.False(t, ok)
}

type mockProvider struct{ name string }

func (m *mockProvider) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	return nil, nil
}
func (m *mockProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	return nil, nil
}
func (m *mockProvider) Name() string { return m.name }

func TestOpenAICompatible_Chat_Success(t *testing.T) {
	wantResp := &domain.OpenAIResponse{
		ID:     "chatcmpl-123",
		Object: "chat.completion",
		Model:  "deepseek-chat",
		Choices: []domain.OpenAIChoice{
			{
				Index:        0,
				Message:      domain.OpenAIMessage{Role: "assistant", Content: "Hello!"},
				FinishReason: "stop",
			},
		},
		Usage: domain.OpenAIUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wantResp)
	}))
	defer server.Close()

	p := NewOpenAICompatible("test", server.URL, 10*time.Second)
	req := &domain.OpenAIRequest{
		Model:    "deepseek-chat",
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}

	got, err := p.Chat(context.Background(), req, "test-key")
	require.NoError(t, err)
	assert.Equal(t, "chatcmpl-123", got.ID)
	assert.Equal(t, "Hello!", got.Choices[0].Message.Content)
}

func TestOpenAICompatible_Chat_ProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "rate limit exceeded"},
		})
	}))
	defer server.Close()

	p := NewOpenAICompatible("test", server.URL, 10*time.Second)
	_, err := p.Chat(context.Background(), &domain.OpenAIRequest{}, "key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "provider rate limited")
}

func TestOpenAICompatible_Chat_UnauthorizedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "invalid api key"},
		})
	}))
	defer server.Close()

	p := NewOpenAICompatible("test", server.URL, 10*time.Second)
	_, err := p.Chat(context.Background(), &domain.OpenAIRequest{}, "bad-key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "provider auth failed")
}

func TestOpenAICompatible_StreamChat_Success(t *testing.T) {
	sseData := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseData))
	}))
	defer server.Close()

	p := NewOpenAICompatible("test", server.URL, 10*time.Second)
	req := &domain.OpenAIRequest{
		Model:    "deepseek-chat",
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
		Stream:   true,
	}

	ch, err := p.StreamChat(context.Background(), req, "test-key")
	require.NoError(t, err)

	var chunks []domain.SSEChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	assert.Len(t, chunks, 4)
	assert.Equal(t, "Hi", chunks[0].Chunk.Choices[0].Delta.Content)
	assert.Equal(t, "!", chunks[1].Chunk.Choices[0].Delta.Content)
	assert.Equal(t, "stop", *chunks[2].Chunk.Choices[0].FinishReason)
	assert.True(t, chunks[3].Done)
}

func TestOpenAICompatible_StreamChat_ProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	p := NewOpenAICompatible("test", server.URL, 10*time.Second)
	_, err := p.StreamChat(context.Background(), &domain.OpenAIRequest{Stream: true}, "key")
	assert.Error(t, err)
}
