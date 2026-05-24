package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
)

func intPtr(v int) *int       { return &v }
func floatPtr(v float64) *float64 { return &v }

func TestOpenAIToAnthropicRequest_BasicRequest(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(1024),
		Messages: []domain.OpenAIMessage{
			{Role: "user", Content: "Hello"},
		},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-20250514", got.Model)
	assert.Equal(t, 1024, got.MaxTokens)
	assert.Len(t, got.Messages, 1)
	assert.Equal(t, "user", got.Messages[0].Role)

	var content string
	json.Unmarshal(got.Messages[0].Content, &content)
	assert.Equal(t, "Hello", content)
}

func TestOpenAIToAnthropicRequest_SystemPrompt(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(512),
		Messages: []domain.OpenAIMessage{
			{Role: "system", Content: "You are helpful"},
			{Role: "user", Content: "Hi"},
		},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 1) // system extracted to top-level field
	assert.NotNil(t, got.System)

	var sys string
	json.Unmarshal(got.System, &sys)
	assert.Equal(t, "You are helpful", sys)
}

func TestOpenAIToAnthropicRequest_DefaultMaxTokens(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []domain.OpenAIMessage{
			{Role: "user", Content: "Hi"},
		},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Equal(t, 4096, got.MaxTokens)
}

func TestOpenAIToAnthropicRequest_AssistantWithToolCalls(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(512),
		Messages: []domain.OpenAIMessage{
			{Role: "user", Content: "What's the weather?"},
			{Role: "assistant", Content: "Let me check.", ToolCalls: []domain.OpenAIToolCall{
				{ID: "call_123", Type: "function", Function: domain.OpenAIFunctionCall{Name: "get_weather", Arguments: `{"city":"Beijing"}`}},
			}},
		},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 2)

	var blocks []domain.ContentBlock
	json.Unmarshal(got.Messages[1].Content, &blocks)
	assert.Len(t, blocks, 2)
	assert.Equal(t, "text", blocks[0].Type)
	assert.Equal(t, "Let me check.", blocks[0].Text)
	assert.Equal(t, "tool_use", blocks[1].Type)
	assert.Equal(t, "call_123", blocks[1].ID)
	assert.Equal(t, "get_weather", blocks[1].Name)
}

func TestOpenAIToAnthropicRequest_ToolResult(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(512),
		Messages: []domain.OpenAIMessage{
			{Role: "user", Content: "Check weather"},
			{Role: "assistant", ToolCalls: []domain.OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: domain.OpenAIFunctionCall{Name: "get_weather", Arguments: `{}`}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "Sunny"},
		},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)

	var blocks []domain.ContentBlock
	json.Unmarshal(got.Messages[2].Content, &blocks)
	assert.Len(t, blocks, 1)
	assert.Equal(t, "tool_result", blocks[0].Type)
	assert.Equal(t, "call_1", blocks[0].ToolUseID)
	assert.Equal(t, `"Sunny"`, string(blocks[0].Content))
}

func TestOpenAIToAnthropicRequest_Tools(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(512),
		Tools: []domain.OpenAITool{
			{Type: "function", Function: domain.OpenAIFunctionDef{
				Name: "get_weather", Description: "Get weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			}},
		},
		Messages: []domain.OpenAIMessage{
			{Role: "user", Content: "Hi"},
		},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)

	var tools []domain.AnthropicTool
	json.Unmarshal(got.Tools, &tools)
	assert.Len(t, tools, 1)
	assert.Equal(t, "get_weather", tools[0].Name)
	assert.Equal(t, "Get weather", tools[0].Description)
}

func TestOpenAIToAnthropicRequest_ToolChoiceAuto(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:       "claude-sonnet-4-20250514",
		MaxTokens:   intPtr(512),
		ToolChoice:  "auto",
		Messages:    []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Equal(t, `{"type":"auto"}`, string(got.ToolChoice))
}

func TestOpenAIToAnthropicRequest_ToolChoiceRequired(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:       "claude-sonnet-4-20250514",
		MaxTokens:   intPtr(512),
		ToolChoice:  "required",
		Messages:    []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Equal(t, `{"type":"any"}`, string(got.ToolChoice))
}

func TestOpenAIToAnthropicRequest_StopSequence_String(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(512),
		Stop:      "END",
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.Equal(t, []string{"END"}, got.StopSequences)
}

func TestOpenAIToAnthropicRequest_User_MappedToMetadata(t *testing.T) {
	req := &domain.OpenAIRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: intPtr(512),
		User:      "user-123",
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}
	got, err := OpenAIToAnthropicRequest(req)
	assert.NoError(t, err)
	assert.NotNil(t, got.Metadata)
	assert.Equal(t, "user-123", got.Metadata.UserID)
}
