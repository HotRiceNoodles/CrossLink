package translator

import (
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestAnthropicToOpenAIResponse_TextOnly(t *testing.T) {
	resp := &domain.AnthropicResponse{
		ID:   "msg_123",
		Type: "message",
		Role: "assistant",
		Content: []domain.ContentBlock{
			{Type: "text", Text: "Hello!"},
		},
		Model:      "claude-sonnet-4-20250514",
		StopReason: "end_turn",
		Usage:      domain.AnthropicUsage{InputTokens: 10, OutputTokens: 5},
	}
	got, err := AnthropicToOpenAIResponse(resp)
	assert.NoError(t, err)
	assert.Equal(t, "msg_123", got.ID)
	assert.Equal(t, "chat.completion", got.Object)
	assert.Equal(t, "claude-sonnet-4-20250514", got.Model)
	assert.Len(t, got.Choices, 1)
	assert.Equal(t, "Hello!", got.Choices[0].Message.Content)
	assert.Equal(t, "stop", got.Choices[0].FinishReason)
	assert.Equal(t, 10, got.Usage.PromptTokens)
	assert.Equal(t, 5, got.Usage.CompletionTokens)
	assert.Equal(t, 15, got.Usage.TotalTokens)
}

func TestAnthropicToOpenAIResponse_ToolUse(t *testing.T) {
	resp := &domain.AnthropicResponse{
		ID:   "msg_456",
		Content: []domain.ContentBlock{
			{Type: "text", Text: "Let me check."},
			{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: []byte(`{"city":"Beijing"}`)},
		},
		StopReason: "tool_use",
		Usage:      domain.AnthropicUsage{InputTokens: 20, OutputTokens: 15},
	}
	got, err := AnthropicToOpenAIResponse(resp)
	assert.NoError(t, err)
	assert.Equal(t, "Let me check.", got.Choices[0].Message.Content)
	assert.Len(t, got.Choices[0].Message.ToolCalls, 1)
	assert.Equal(t, "toolu_1", got.Choices[0].Message.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", got.Choices[0].Message.ToolCalls[0].Function.Name)
	assert.Equal(t, `{"city":"Beijing"}`, got.Choices[0].Message.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "tool_calls", got.Choices[0].FinishReason)
}

func TestAnthropicToOpenAIResponse_StopReasons(t *testing.T) {
	tests := []struct {
		stopReason   string
		wantFinish   string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
		{"unknown", "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.stopReason, func(t *testing.T) {
			resp := &domain.AnthropicResponse{
				Content:    []domain.ContentBlock{{Type: "text", Text: "ok"}},
				StopReason: tt.stopReason,
			}
			got, err := AnthropicToOpenAIResponse(resp)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantFinish, got.Choices[0].FinishReason)
		})
	}
}
