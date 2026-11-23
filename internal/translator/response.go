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

	// 类二 M-B: carried thinking blocks (from the anthropic upstream response)
	// replay VERBATIM — signature intact — ahead of the rebuilt text/tool
	// blocks; the reasoningContent-derived synthesis (which has no signature)
	// is skipped in that case. No carry → existing behavior.
	var thinkingBlocks []domain.ContentBlock
	if len(resp.ThinkingBlocks) > 0 {
		thinkingBlocks = resp.ThinkingBlocks
	} else if reasoningContent != "" {
		thinkingBlocks = nil // synthesized inside buildContentBlocks (legacy path)
	}
	if len(thinkingBlocks) > 0 {
		rest := buildContentBlocks(content, "", toolCalls) // text/tool only, no synthesized thinking
		contentBlocks := make([]domain.ContentBlock, 0, len(thinkingBlocks)+len(rest))
		contentBlocks = append(contentBlocks, thinkingBlocks...)
		contentBlocks = append(contentBlocks, rest...)
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
				CacheReadInputTokens: func() int {
					if resp.Usage.PromptTokensDetails != nil {
						return resp.Usage.PromptTokensDetails.CachedTokens
					}
					return 0
				}(),
				CacheCreationInputTokens: resp.Usage.CacheCreationTokens,
			},
		}, nil
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
			CacheReadInputTokens: func() int {
				if resp.Usage.PromptTokensDetails != nil {
					return resp.Usage.PromptTokensDetails.CachedTokens
				}
				return 0
			}(),
			// Cache-creation visibility: the internal carry (private on the
			// OpenAI wire) surfaces here so anthropic-inbound clients see the
			// full cache accounting (writes bill at 1.25× on Anthropic).
			CacheCreationInputTokens: resp.Usage.CacheCreationTokens,
		},
	}, nil
}

func buildContentBlocks(text, reasoningContent string, toolCalls []domain.OpenAIToolCall) []domain.ContentBlock {
	var blocks []domain.ContentBlock

	// Thinking block comes first if present — the anthropic wire keys the
	// reasoning text as "thinking" (not "text"); the pre-类二 code emitted a
	// "text"-keyed block that upstreams would reject/misread.
	if reasoningContent != "" {
		blocks = append(blocks, domain.ContentBlock{
			Type:     "thinking",
			Thinking: reasoningContent,
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
