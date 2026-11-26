package translator

import (
	"github.com/crosslink/internal/domain"
)

// AnthropicToOpenAIResponse translates an Anthropic Messages API response into OpenAI format.
func AnthropicToOpenAIResponse(resp *domain.AnthropicResponse) (*domain.OpenAIResponse, error) {
	var content string
	var reasoningContent string
	var toolCalls []domain.OpenAIToolCall

	// Thinking-fidelity carry (M-B): collect thinking/redacted_thinking blocks
	// as VALUES — the signature (and redacted data) bind to the block's
	// content, not to our serialization, so value-level preservation through
	// struct fields is sufficient and exact. reasoningContent text mapping
	// continues for openai-inbound clients.
	var thinkingBlocks []domain.ContentBlock
	// Phase-2 block fidelity: unrecognized-typed blocks (e.g. compaction
	// blocks under beta) carry their Raw bytes through the internal
	// representation instead of being silently dropped.
	var extraBlocks []domain.ContentBlock
	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			reasoningContent += block.Thinking
			thinkingBlocks = append(thinkingBlocks, block)
		case "redacted_thinking":
			thinkingBlocks = append(thinkingBlocks, block)
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
		default:
			if len(block.Raw) > 0 {
				extraBlocks = append(extraBlocks, block)
			}
		}
	}

	finishReason := stopReasonToFinishReason(resp.StopReason)

	return &domain.OpenAIResponse{
		ID:     resp.ID,
		Object: "chat.completion",
		Model:  resp.Model,
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
		// Thinking-fidelity carry (M-B): thinking/redacted blocks ride to the
		// anthropic-facing response assembly (OpenAIToAnthropic), which emits
		// them verbatim (signature intact) instead of synthesizing
		// signature-less blocks from reasoningContent.
		ThinkingBlocks: thinkingBlocks,
		// Phase-2 block fidelity: unrecognized-typed blocks ride with their
		// Raw bytes for verbatim re-emission.
		AnthropicExtraBlocks: extraBlocks,
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
			AnthropicIterations: resp.Usage.Iterations,
		},
	}, nil
}

var stopReasonToFinishReasonMap = map[string]string{
	"end_turn":      "stop",
	"max_tokens":    "length",
	"stop_sequence": "stop",
	"tool_use":      "tool_calls",
	// Phase-2 refusal fidelity (checklist §3 B 档 fallbacks 前置): an
	// upstream refusal must not round-trip into a normal end_turn — the
	// openai-internal pair of a refusal is content_filter.
	"refusal": "content_filter",
}

func stopReasonToFinishReason(reason string) string {
	if v, ok := stopReasonToFinishReasonMap[reason]; ok {
		return v
	}
	return "stop"
}
