package translator

import (
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestAnthropicToOpenAIResponse_CacheTokens(t *testing.T) {
	resp := &domain.AnthropicResponse{
		ID:   "msg_cache",
		Type: "message",
		Role: "assistant",
		Content: []domain.ContentBlock{
			{Type: "text", Text: "Hello!"},
		},
		Model:      "claude-sonnet-4-20250514",
		StopReason: "end_turn",
		Usage: domain.AnthropicUsage{
			InputTokens:          100,
			OutputTokens:         20,
			CacheReadInputTokens: 45,
		},
	}
	got, err := AnthropicToOpenAIResponse(resp)
	assert.NoError(t, err)
	assert.Equal(t, 100, got.Usage.PromptTokens)
	assert.Equal(t, 20, got.Usage.CompletionTokens)
	assert.NotNil(t, got.Usage.PromptTokensDetails)
	assert.Equal(t, 45, got.Usage.PromptTokensDetails.CachedTokens)
}

func TestAnthropicToOpenAIResponse_NoCacheTokens(t *testing.T) {
	resp := &domain.AnthropicResponse{
		Content: []domain.ContentBlock{
			{Type: "text", Text: "No cache"},
		},
		Usage: domain.AnthropicUsage{
			InputTokens:  50,
			OutputTokens: 10,
		},
	}
	got, err := AnthropicToOpenAIResponse(resp)
	assert.NoError(t, err)
	assert.Nil(t, got.Usage.PromptTokensDetails)
}
