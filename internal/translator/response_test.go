package translator

import (
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIToAnthropic_BasicResponse_Success(t *testing.T) {
	resp := &domain.OpenAIResponse{
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
		Usage: domain.OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}

	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)

	assert.Equal(t, "message", got.Type)
	assert.Equal(t, "assistant", got.Role)
	assert.Equal(t, "claude-sonnet-4-20250514", got.Model)
	assert.Equal(t, "end_turn", got.StopReason)
	assert.Nil(t, got.StopSequence)
	assert.Len(t, got.Content, 1)
	assert.Equal(t, "text", got.Content[0].Type)
	assert.Equal(t, "Hello!", got.Content[0].Text)
	assert.Contains(t, got.ID, "msg_")
	assert.Equal(t, 10, got.Usage.InputTokens)
	assert.Equal(t, 5, got.Usage.OutputTokens)
}

func TestOpenAIToAnthropic_FinishReasonLength_MaxTokens(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{FinishReason: "length", Message: domain.OpenAIMessage{Content: "cut off"}},
		},
		Usage: domain.OpenAIUsage{},
	}
	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Equal(t, "max_tokens", got.StopReason)
}

func TestOpenAIToAnthropic_FinishReasonContentFilter_EndTurn(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{FinishReason: "content_filter", Message: domain.OpenAIMessage{Content: ""}},
		},
		Usage: domain.OpenAIUsage{},
	}
	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Equal(t, "end_turn", got.StopReason)
}

func TestOpenAIToAnthropic_FinishReasonUnknown_EndTurn(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{FinishReason: "unknown_reason", Message: domain.OpenAIMessage{Content: "test"}},
		},
		Usage: domain.OpenAIUsage{},
	}
	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Equal(t, "end_turn", got.StopReason)
}

func TestOpenAIToAnthropic_Usage_Mapped(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{FinishReason: "stop", Message: domain.OpenAIMessage{Content: "ok"}},
		},
		Usage: domain.OpenAIUsage{
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		},
	}
	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Equal(t, 100, got.Usage.InputTokens)
	assert.Equal(t, 50, got.Usage.OutputTokens)
}

func TestOpenAIToAnthropic_EmptyContent_EmptyTextBlock(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{FinishReason: "stop", Message: domain.OpenAIMessage{Content: ""}},
		},
		Usage: domain.OpenAIUsage{},
	}
	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Len(t, got.Content, 1)
	assert.Equal(t, "", got.Content[0].Text)
}

func TestOpenAIToAnthropic_NoChoices_EmptyContent(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{},
		Usage:   domain.OpenAIUsage{},
	}
	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Len(t, got.Content, 1)
	assert.Equal(t, "", got.Content[0].Text)
	assert.Equal(t, "end_turn", got.StopReason)
}

func TestFinishReasonToStopReason_ToolCalls(t *testing.T) {
	assert.Equal(t, "tool_use", finishReasonToStopReason("tool_calls"))
}

// --- Tool Use response translation tests ---

func TestOpenAIToAnthropic_ToolCallsResponse(t *testing.T) {
	resp := &domain.OpenAIResponse{
		ID:     "chatcmpl-123",
		Object: "chat.completion",
		Model:  "deepseek-chat",
		Choices: []domain.OpenAIChoice{
			{
				Index:        0,
				FinishReason: "tool_calls",
				Message: domain.OpenAIMessage{
					Role:    "assistant",
					Content: "",
					ToolCalls: []domain.OpenAIToolCall{
						{
							ID:   "call_abc123",
							Type: "function",
							Function: domain.OpenAIFunctionCall{
								Name:      "get_weather",
								Arguments: `{"city":"Beijing"}`,
							},
						},
					},
				},
			},
		},
		Usage: domain.OpenAIUsage{
			PromptTokens:     50,
			CompletionTokens: 20,
		},
	}

	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Equal(t, "tool_use", got.StopReason)

	// Should have only tool_use block (no empty text block)
	assert.Len(t, got.Content, 1)
	assert.Equal(t, "tool_use", got.Content[0].Type)
	assert.Equal(t, "call_abc123", got.Content[0].ID)
	assert.Equal(t, "get_weather", got.Content[0].Name)
	assert.Equal(t, `{"city":"Beijing"}`, string(got.Content[0].Input))
}

func TestOpenAIToAnthropic_ToolCallsWithText(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{
				FinishReason: "tool_calls",
				Message: domain.OpenAIMessage{
					Role:    "assistant",
					Content: "Let me check that.",
					ToolCalls: []domain.OpenAIToolCall{
						{
							ID:   "call_1",
							Type: "function",
							Function: domain.OpenAIFunctionCall{
								Name:      "search",
								Arguments: `{"q":"test"}`,
							},
						},
					},
				},
			},
		},
		Usage: domain.OpenAIUsage{},
	}

	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Equal(t, "tool_use", got.StopReason)
	assert.Len(t, got.Content, 2)
	assert.Equal(t, "Let me check that.", got.Content[0].Text)
	assert.Equal(t, "tool_use", got.Content[1].Type)
}

func TestOpenAIToAnthropic_MultipleToolCalls(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{
			{
				FinishReason: "tool_calls",
				Message: domain.OpenAIMessage{
					Role:    "assistant",
					Content: "",
					ToolCalls: []domain.OpenAIToolCall{
						{ID: "call_1", Type: "function", Function: domain.OpenAIFunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}},
						{ID: "call_2", Type: "function", Function: domain.OpenAIFunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}},
					},
				},
			},
		},
		Usage: domain.OpenAIUsage{},
	}

	got, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	assert.Len(t, got.Content, 2) // 2 tool_use blocks, no empty text
	assert.Equal(t, "read_file", got.Content[0].Name)
	assert.Equal(t, "call_1", got.Content[0].ID)
	assert.Equal(t, "read_file", got.Content[1].Name)
	assert.Equal(t, "call_2", got.Content[1].ID)
}
