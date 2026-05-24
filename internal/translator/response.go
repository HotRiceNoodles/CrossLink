package translator

import (
	"github.com/crosslink/internal/domain"
)

func OpenAIToAnthropic(resp *domain.OpenAIResponse, requestedModel string) (*domain.AnthropicResponse, error) {
	var content string
	var reasoningContent string
	var finishReason string
	var toolCalls []domain.OpenAIToolCall

	if len(resp.Choices) > 0 {
		content = domain.ContentText(resp.Choices[0].Message.Content)
		reasoningContent = resp.Choices[0].Message.ReasoningContent
		finishReason = resp.Choices[0].FinishReason
		toolCalls = resp.Choices[0].Message.ToolCalls
	}

	contentBlocks := buildContentBlocks(content, reasoningContent, toolCalls)

	return &domain.AnthropicResponse{
		ID:           generateMessageID(),
		Type:         "message",
		Role:         "assistant",
		Content:      contentBlocks,
		Model:        requestedModel,
		StopReason:   finishReasonToStopReason(finishReason),
		StopSequence: nil,
		Usage: domain.AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}, nil
}

func buildContentBlocks(text, reasoningContent string, toolCalls []domain.OpenAIToolCall) []domain.ContentBlock {
	var blocks []domain.ContentBlock

	// Thinking block comes first if present
	if reasoningContent != "" {
		blocks = append(blocks, domain.ContentBlock{
			Type: "thinking",
			Text: reasoningContent,
		})
	}

	if text != "" || len(toolCalls) == 0 {
		blocks = append(blocks, domain.ContentBlock{
			Type: "text",
			Text: text,
		})
	}

	for _, tc := range toolCalls {
		blocks = append(blocks, domain.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: jsonString(tc.Function.Arguments),
		})
	}

	return blocks
}

// jsonString returns the JSON string as-is, defaulting empty strings to "{}".
// This ensures tool_use arguments always have a valid JSON object.
func jsonString(s string) []byte {
	if s == "" {
		return []byte("{}")
	}
	return []byte(s)
}
