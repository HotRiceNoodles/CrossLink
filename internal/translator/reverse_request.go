package translator

import (
	"encoding/json"
	"strings"

	"github.com/crosslink/internal/domain"
)

// OpenAIToAnthropicRequest translates an OpenAI-format request into an Anthropic Messages API request.
func OpenAIToAnthropicRequest(req *domain.OpenAIRequest) (*domain.AnthropicRequest, error) {
	var systemParts []string
	var msgs []domain.AnthropicMessage

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			systemParts = append(systemParts, domain.ContentText(msg.Content))
		case "user":
			content, _ := json.Marshal(msg.Content)
			msgs = append(msgs, domain.AnthropicMessage{Role: "user", Content: content})
		case "assistant":
			content, _ := json.Marshal(assistantToContentBlocks(msg))
			msgs = append(msgs, domain.AnthropicMessage{Role: "assistant", Content: content})
		case "tool":
			content, _ := json.Marshal(toolResultToContentBlocks(msg))
			msgs = append(msgs, domain.AnthropicMessage{Role: "user", Content: content})
		}
	}

	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		maxTokens = *req.MaxCompletionTokens
	}

	anthropicReq := &domain.AnthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		Messages:    msgs,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if len(systemParts) > 0 {
		sys, _ := json.Marshal(strings.Join(systemParts, "\n"))
		anthropicReq.System = sys
	}

	if len(req.Tools) > 0 {
		tools := make([]domain.AnthropicTool, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = domain.AnthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			}
		}
		toolsJSON, _ := json.Marshal(tools)
		anthropicReq.Tools = toolsJSON
	}

	if req.ToolChoice != nil {
		anthropicReq.ToolChoice = reverseToolChoice(req.ToolChoice)
	}

	if req.Stop != nil {
		anthropicReq.StopSequences = reverseStopSequences(req.Stop)
	}

	if req.User != "" {
		anthropicReq.Metadata = &domain.AnthropicMetadata{UserID: req.User}
	}

	if len(req.Thinking) > 0 {
		anthropicReq.Thinking = req.Thinking
	}

	return anthropicReq, nil
}

func assistantToContentBlocks(msg domain.OpenAIMessage) []domain.ContentBlock {
	var blocks []domain.ContentBlock
	if text := domain.ContentText(msg.Content); text != "" {
		blocks = append(blocks, domain.ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, domain.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: jsonString(tc.Function.Arguments),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, domain.ContentBlock{Type: "text", Text: ""})
	}
	return blocks
}

func toolResultToContentBlocks(msg domain.OpenAIMessage) []domain.ContentBlock {
	block := domain.ContentBlock{
		Type:      "tool_result",
		ToolUseID: msg.ToolCallID,
	}
	if msg.Content != nil {
		content, _ := json.Marshal(msg.Content)
		block.Content = content
	}
	return []domain.ContentBlock{block}
}

func reverseToolChoice(tc any) json.RawMessage {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			result, _ := json.Marshal(map[string]string{"type": "auto"})
			return result
		case "required":
			result, _ := json.Marshal(map[string]string{"type": "any"})
			return result
		}
	case map[string]any:
		if v["type"] == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					result, _ := json.Marshal(map[string]string{"type": "tool", "name": name})
					return result
				}
			}
		}
	}
	return nil
}

func reverseStopSequences(stop any) []string {
	switch v := stop.(type) {
	case string:
		return []string{v}
	case []any:
		result := make([]string, 0, len(v))
		for _, s := range v {
			if str, ok := s.(string); ok {
				result = append(result, str)
			}
		}
		return result
	}
	return nil
}
