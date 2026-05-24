package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestAnthropicToOpenAI_BasicRequest_Success(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hello"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "deepseek-chat", got.Model)
	assert.Equal(t, 1024, *got.MaxTokens)
	assert.Len(t, got.Messages, 1)
	assert.Equal(t, "user", got.Messages[0].Role)
	assert.Equal(t, "Hello", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_SystemString_InsertedAtFront(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		System:    raw(`"You are helpful"`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 2)
	assert.Equal(t, "system", got.Messages[0].Role)
	assert.Equal(t, "You are helpful", got.Messages[0].Content)
	assert.Equal(t, "user", got.Messages[1].Role)
}

func TestAnthropicToOpenAI_SystemBlocks_JoinedText(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		System: raw(`[{"type":"text","text":"Line 1"},{"type":"text","text":"Line 2"}]`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "Line 1\nLine 2", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_ContentBlocks_ExtractedText(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`[{"type":"text","text":"Hello"},{"type":"text","text":"World"}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "Hello\nWorld", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_SingleStopSequence_String(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:         "claude-sonnet-4-20250514",
		MaxTokens:     512,
		StopSequences: []string{"END"},
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "END", got.Stop)
}

func TestAnthropicToOpenAI_MultipleStopSequences_Array(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:         "claude-sonnet-4-20250514",
		MaxTokens:     512,
		StopSequences: []string{"END", "STOP"},
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, []string{"END", "STOP"}, got.Stop)
}

func TestAnthropicToOpenAI_TemperatureAndTopP_Passed(t *testing.T) {
	temp := 0.7
	topP := 0.9
	req := &domain.AnthropicRequest{
		Model:       "claude-sonnet-4-20250514",
		MaxTokens:   512,
		Temperature: &temp,
		TopP:        &topP,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, &temp, got.Temperature)
	assert.Equal(t, &topP, got.TopP)
}

func TestAnthropicToOpenAI_MetadataUserID_MappedToUser(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Metadata:  &domain.AnthropicMetadata{UserID: "user-123"},
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "user-123", got.User)
}

func TestAnthropicToOpenAI_Tools_SilentlyStripped(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Tools:     raw(`[{"name":"get_weather"}]`),
		ToolChoice: raw(`{"type":"auto"}`),
		Thinking:  raw(`{"type":"enabled","budget_tokens":10000}`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Nil(t, got.Stop)
	assert.Equal(t, "user", got.Messages[0].Role)
	assert.Equal(t, "Hi", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_MissingModel_Error(t *testing.T) {
	req := &domain.AnthropicRequest{
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	_, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.ErrorIs(t, err, ErrMissingModel)
}

func TestAnthropicToOpenAI_MissingMessages_Error(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
	}
	_, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.ErrorIs(t, err, ErrMissingMessages)
}

func TestAnthropicToOpenAI_MissingMaxTokens_Error(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	_, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.ErrorIs(t, err, ErrMissingMaxTokens)
}

func TestAnthropicToOpenAI_InvalidSystemJSON_Ignored(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		System:    raw(`{invalid json}`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 1)
	assert.Equal(t, "user", got.Messages[0].Role)
}

func TestAnthropicToOpenAI_InvalidContentJSON_FallbackRaw(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`{invalid}`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "{invalid}", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_NilContent_EmptyString(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: nil},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_Stream_PassedWithStreamOptions(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Stream:    true,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.True(t, got.Stream)
	assert.NotNil(t, got.StreamOptions)
	assert.True(t, got.StreamOptions.IncludeUsage)
}

// --- Tool Use request translation tests ---

func TestAnthropicToOpenAI_Tools_Translated(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Tools:     raw(`[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"What's the weather?"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Tools, 1)
	assert.Equal(t, "function", got.Tools[0].Type)
	assert.Equal(t, "get_weather", got.Tools[0].Function.Name)
	assert.Equal(t, "Get weather", got.Tools[0].Function.Description)
	assert.Equal(t, `{"type":"object","properties":{"city":{"type":"string"}}}`, string(got.Tools[0].Function.Parameters))
}

func TestAnthropicToOpenAI_ToolChoiceAuto(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:      "claude-sonnet-4-20250514",
		MaxTokens:  512,
		ToolChoice: raw(`{"type":"auto"}`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "auto", got.ToolChoice)
}

func TestAnthropicToOpenAI_ToolChoiceRequired(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:      "claude-sonnet-4-20250514",
		MaxTokens:  512,
		ToolChoice: raw(`{"type":"any"}`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Equal(t, "required", got.ToolChoice)
}

func TestAnthropicToOpenAI_ToolChoiceSpecific(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:      "claude-sonnet-4-20250514",
		MaxTokens:  512,
		ToolChoice: raw(`{"type":"tool","name":"get_weather"}`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Hi"`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	tc, ok := got.ToolChoice.(map[string]any)
	assert.True(t, ok, "expected map tool_choice, got %T", got.ToolChoice)
	assert.Equal(t, "get_weather", tc["function"].(map[string]any)["name"])
}

func TestAnthropicToOpenAI_ToolUseInAssistantMessage(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"What's the weather?"`)},
			{Role: "assistant", Content: raw(`[{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Beijing"}}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	// user msg + assistant msg with tool_calls
	assert.Len(t, got.Messages, 2)
	assistant := got.Messages[1]
	assert.Equal(t, "assistant", assistant.Role)
	assert.Len(t, assistant.ToolCalls, 1)
	assert.Equal(t, "toolu_123", assistant.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", assistant.ToolCalls[0].Function.Name)
	assert.Equal(t, `{"city":"Beijing"}`, assistant.ToolCalls[0].Function.Arguments)
}

func TestAnthropicToOpenAI_ToolResultInUserMessage(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"What's the weather?"`)},
			{Role: "assistant", Content: raw(`[{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Beijing"}}]`)},
			{Role: "user", Content: raw(`[{"type":"tool_result","tool_use_id":"toolu_123","content":"Sunny, 25°C"}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	// user + assistant(tool_calls) + user(tool_result → role:tool)
	assert.Len(t, got.Messages, 3)
	toolResult := got.Messages[2]
	assert.Equal(t, "tool", toolResult.Role)
	assert.Equal(t, "toolu_123", toolResult.ToolCallID)
	assert.Equal(t, "Sunny, 25°C", toolResult.Content)
}

func TestAnthropicToOpenAI_MixedTextAndToolUse(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Check weather"`)},
			{Role: "assistant", Content: raw(`[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Shanghai"}}]`)},
			{Role: "user", Content: raw(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"Rainy, 18°C"}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 3)

	assistant := got.Messages[1]
	assert.Equal(t, "Let me check.", assistant.Content)
	assert.Len(t, assistant.ToolCalls, 1)

	toolResult := got.Messages[2]
	assert.Equal(t, "tool", toolResult.Role)
	assert.Equal(t, "toolu_1", toolResult.ToolCallID)
}
