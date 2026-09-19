package translator

import "github.com/crosslink/internal/domain"

// AnthropicToOpenAIResponse translates an Anthropic Messages API response into OpenAI format.
func AnthropicToOpenAIResponse(resp *domain.AnthropicResponse) (*domain.OpenAIResponse, error) {
	var content string
	var reasoningContent string
	var toolCalls []domain.OpenAIToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			reasoningContent += block.Text
		case "text":
			content += block.Text
		case "tool_use":
			toolCalls = append(toolCalls, domain.OpenAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: domain.OpenAIFunctionCall{
					Name:      block.Name,
					Arguments: string(block.Input),
				},
			})
		}
	}

	finishReason := stopReasonToFinishReason(resp.StopReason)

	return &domain.OpenAIResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Model:   resp.Model,
		Choices: []domain.OpenAIChoice{
			{
				Index: 0,
				Message: domain.OpenAIMessage{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: reasoningContent,
					ToolCalls:        toolCalls,
				},
				FinishReason: finishReason,
			},
		},
		Usage: domain.OpenAIUsage{
			// OpenAI semantics: prompt_tokens includes cached tokens. Anthropic's
			// input_tokens excludes cache reads/writes, so fold them in here —
			// billing (and the client-facing forward translation) then sees one
			// consistent total.
			PromptTokens:     resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.OutputTokens,
			PromptTokensDetails: func() *domain.PromptTokensDetails {
				if resp.Usage.CacheReadInputTokens > 0 {
					return &domain.PromptTokensDetails{CachedTokens: resp.Usage.CacheReadInputTokens}
				}
				return nil
			}(),
			CacheCreationTokens: resp.Usage.CacheCreationInputTokens,
		},
	}, nil
}

var stopReasonToFinishReasonMap = map[string]string{
	"end_turn":      "stop",
	"max_tokens":    "length",
	"stop_sequence": "stop",
	"tool_use":      "tool_calls",
}

func stopReasonToFinishReason(reason string) string {
	if v, ok := stopReasonToFinishReasonMap[reason]; ok {
		return v
	}
	return "stop"
}
