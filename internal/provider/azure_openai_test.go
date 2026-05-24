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

func TestAzureOpenAI_Chat_Success(t *testing.T) {
	wantResp := &domain.OpenAIResponse{
		ID:     "chatcmpl-123",
		Object: "chat.completion",
		Model:  "gpt-4",
		Choices: []domain.OpenAIChoice{
			{Index: 0, Message: domain.OpenAIMessage{Role: "assistant", Content: "Hello!"}, FinishReason: "stop"},
		},
		Usage: domain.OpenAIUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Contains(t, r.URL.Path, "/openai/deployments/my-gpt4/chat/completions")
		assert.Equal(t, "2024-02-01", r.URL.Query().Get("api-version"))
		assert.Equal(t, "azure-key", r.Header.Get("api-key"))
		assert.Empty(t, r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wantResp)
	}))
	defer server.Close()

	extraConfig, _ := json.Marshal(map[string]string{
		"deployment_name": "my-gpt4",
		"api_version":     "2024-02-01",
	})
	p, err := NewAzureOpenAIProvider("azure", server.URL, "azure-key", extraConfig, 10*time.Second)
	require.NoError(t, err)

	got, err := p.Chat(context.Background(), &domain.OpenAIRequest{
		Model:    "gpt-4",
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}, "azure-key")
	require.NoError(t, err)
	assert.Equal(t, "Hello!", got.Choices[0].Message.Content)
}

func TestAzureOpenAI_MissingDeployment(t *testing.T) {
	_, err := NewAzureOpenAIProvider("azure", "https://example.com", "key", nil, 10*time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "deployment_name is required")
}

func TestAzureOpenAI_StreamChat_Success(t *testing.T) {
	sseData := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "api-key", r.Header.Get("api-key"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseData))
	}))
	defer server.Close()

	extraConfig, _ := json.Marshal(map[string]string{"deployment_name": "my-deploy"})
	p, err := NewAzureOpenAIProvider("azure", server.URL, "key", extraConfig, 10*time.Second)
	require.NoError(t, err)

	ch, err := p.StreamChat(context.Background(), &domain.OpenAIRequest{
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
		Stream:   true,
	}, "api-key")
	require.NoError(t, err)

	var chunks []domain.SSEChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	assert.GreaterOrEqual(t, len(chunks), 2)
	assert.True(t, chunks[len(chunks)-1].Done)
}
