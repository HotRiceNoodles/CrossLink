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
		System:    raw(`[{"type":"text","text":"Line 1"},{"type":"text","text":"Line 2"}]`),
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
		Model:      "claude-sonnet-4-20250514",
		MaxTokens:  512,
		Tools:      raw(`[{"name":"get_weather"}]`),
		ToolChoice: raw(`{"type":"auto"}`),
		Thinking:   raw(`{"type":"enabled","budget_tokens":10000}`),
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

func TestAnthropicToOpenAI_ImageBlocks_ConvertedToImageURLParts(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`[
				{"type":"text","text":"What is this?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},
				{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}
			]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 1)

	content, ok := got.Messages[0].Content.([]any)
	assert.True(t, ok, "content should be a parts array")
	assert.Len(t, content, 3)

	textPart, _ := content[0].(map[string]any)
	assert.Equal(t, "text", textPart["type"])
	assert.Equal(t, "What is this?", textPart["text"])

	imagePart, _ := content[1].(map[string]any)
	assert.Equal(t, "image_url", imagePart["type"])
	imageURL, _ := imagePart["image_url"].(map[string]any)
	assert.Equal(t, "data:image/png;base64,aGVsbG8=", imageURL["url"])

	imagePart2, _ := content[2].(map[string]any)
	assert.Equal(t, "image_url", imagePart2["type"])
	imageURL2, _ := imagePart2["image_url"].(map[string]any)
	assert.Equal(t, "https://example.com/cat.png", imageURL2["url"])

	// Guardrails and cache keys extract text via ContentText — it must keep
	// working on the converted parts array.
	assert.Equal(t, "What is this?", domain.ContentText(got.Messages[0].Content))
}

func TestAnthropicToOpenAI_ImageOnlyUserMessage(t *testing.T) {
	// Previously an image-only message collapsed to Content:"" — the image
	// vanished silently.
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"eHg="}}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 1)

	content, ok := got.Messages[0].Content.([]any)
	assert.True(t, ok, "content should be a parts array")
	assert.Len(t, content, 1)
	imagePart, _ := content[0].(map[string]any)
	assert.Equal(t, "image_url", imagePart["type"])
	imageURL, _ := imagePart["image_url"].(map[string]any)
	assert.Equal(t, "data:image/jpeg;base64,eHg=", imageURL["url"])
}

func TestAnthropicToOpenAI_ImageBlock_UnusableSourceDropped(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`[
				{"type":"text","text":"hi"},
				{"type":"image"},
				{"type":"image","source":{"type":"file","file_id":"file-123"}}
			]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 1)

	// Only the text part survives; unusable images are dropped, text still
	// collapses to a plain string.
	assert.Equal(t, "hi", got.Messages[0].Content)
}

func TestAnthropicToOpenAI_ToolResultWithImage_ImageInTrailingMessage(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"Check this screenshot"`)},
			{Role: "assistant", Content: raw(`[{"type":"tool_use","id":"toolu_1","name":"take_screenshot","input":{}}]`)},
			{Role: "user", Content: raw(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"done"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"eHg="}}
			]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	// user + assistant(tool_calls) + tool result + trailing user image message
	assert.Len(t, got.Messages, 4)

	toolResult := got.Messages[2]
	assert.Equal(t, "tool", toolResult.Role)
	assert.Equal(t, "toolu_1", toolResult.ToolCallID)

	trailing := got.Messages[3]
	assert.Equal(t, "user", trailing.Role)
	content, ok := trailing.Content.([]any)
	assert.True(t, ok)
	assert.Len(t, content, 1)
}

func TestAnthropicToOpenAI_ToolResultContentWithImage_ImageSurvives(t *testing.T) {
	// Images INSIDE tool_result content belong to the tool output: they must
	// ride along the tool message (parts array), not vanish. Previously they
	// were silently dropped by the text-only extraction.
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: raw(`"check the screen"`)},
			{Role: "assistant", Content: raw(`[{"type":"tool_use","id":"toolu_1","name":"take_screenshot","input":{}}]`)},
			{Role: "user", Content: raw(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"eHg="}},
				{"type":"text","text":"screenshot attached"}
			]}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	// user + assistant(tool_calls) + tool message
	assert.Len(t, got.Messages, 3)

	tool := got.Messages[2]
	assert.Equal(t, "tool", tool.Role)
	assert.Equal(t, "toolu_1", tool.ToolCallID)

	parts, ok := tool.Content.([]any)
	assert.True(t, ok, "tool content should be a parts array when the result carries images")
	assert.Len(t, parts, 2)

	textPart, _ := parts[0].(map[string]any)
	assert.Equal(t, "text", textPart["type"])
	assert.Equal(t, "screenshot attached", textPart["text"])

	imagePart, _ := parts[1].(map[string]any)
	assert.Equal(t, "image_url", imagePart["type"])
	imageURL, _ := imagePart["image_url"].(map[string]any)
	assert.Equal(t, "data:image/png;base64,eHg=", imageURL["url"])

	// Text extraction (guardrails, cache keys) keeps working on the array.
	assert.Equal(t, "screenshot attached", domain.ContentText(tool.Content))
}

func TestAnthropicToOpenAI_ToolResultTextOnlyContent_StaysString(t *testing.T) {
	// Backward compatibility: text-only tool_result content must remain a
	// plain string, byte-identical to the pre-image-support behavior.
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 512,
		Messages: []domain.AnthropicMessage{
			{Role: "assistant", Content: raw(`[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]`)},
			{Role: "user", Content: raw(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"text","text":"line one"},
				{"type":"text","text":"line two"}
			]}]`)},
		},
	}
	got, err := AnthropicToOpenAI(req, "deepseek-chat")
	assert.NoError(t, err)
	assert.Len(t, got.Messages, 2)
	assert.Equal(t, "line one\nline two", got.Messages[1].Content)
	assert.Equal(t, "tool", got.Messages[1].Role)
}
