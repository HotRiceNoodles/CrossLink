package translator

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/crosslink/internal/domain"
)

var (
	ErrMissingModel     = errors.New("model is required")
	ErrMissingMessages  = errors.New("messages is required")
	ErrMissingMaxTokens = errors.New("max_tokens is required")
)

func AnthropicToOpenAI(req *domain.AnthropicRequest, modelName string) (*domain.OpenAIRequest, error) {
	if req.Model == "" {
		return nil, ErrMissingModel
	}
	if len(req.Messages) == 0 {
		return nil, ErrMissingMessages
	}
	if req.MaxTokens <= 0 {
		return nil, ErrMissingMaxTokens
	}

	messages := make([]domain.OpenAIMessage, 0, len(req.Messages)+1)

	if sys := extractSystemPrompt(req.System); sys != "" {
		messages = append(messages, domain.OpenAIMessage{Role: "system", Content: sys})
	}

	for _, msg := range req.Messages {
		openaiMsgs := translateMessage(msg)
		messages = append(messages, openaiMsgs...)
	}

	oaiReq := &domain.OpenAIRequest{
		Model:       modelName,
		Messages:    messages,
		MaxTokens:   &req.MaxTokens,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if req.Stream {
		oaiReq.StreamOptions = &domain.StreamOptions{IncludeUsage: true}
	}

	if len(req.StopSequences) > 0 {
		oaiReq.Stop = translateStopSequences(req.StopSequences)
	}

	if req.Metadata != nil && req.Metadata.UserID != "" {
		oaiReq.User = req.Metadata.UserID
	}

	// Tool Use translation
	if len(req.Tools) > 0 {
		tools, err := translateTools(req.Tools)
		if err == nil {
			oaiReq.Tools = tools
		}
	}
	if req.ToolChoice != nil {
		oaiReq.ToolChoice = translateToolChoice(req.ToolChoice)
	}
	if len(req.Thinking) > 0 {
		oaiReq.Thinking = req.Thinking
	}

	return oaiReq, nil
}

func translateMessage(msg domain.AnthropicMessage) []domain.OpenAIMessage {
	if msg.Content == nil {
		return []domain.OpenAIMessage{{Role: msg.Role, Content: ""}}
	}

	// Try string content first
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return []domain.OpenAIMessage{{Role: msg.Role, Content: s}}
	}

	// Try content block array
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return []domain.OpenAIMessage{{Role: msg.Role, Content: string(msg.Content)}}
	}

	return translateContentBlocks(msg.Role, blocks)
}

func translateContentBlocks(role string, blocks []domain.ContentBlock) []domain.OpenAIMessage {
	// Collect text blocks and tool_use/tool_result blocks separately
	var textParts []string
	var toolCalls []domain.OpenAIToolCall
	var results []domain.OpenAIMessage

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			toolCalls = append(toolCalls, domain.OpenAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: domain.OpenAIFunctionCall{
					Name:      b.Name,
					Arguments: string(b.Input),
				},
			})
		case "tool_result":
			content := extractToolResultContent(b.Content)
			results = append(results, domain.OpenAIMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: b.ToolUseID,
			})
		}
	}

	// If only text blocks, return a simple message
	if len(toolCalls) == 0 && len(results) == 0 {
		return []domain.OpenAIMessage{{Role: role, Content: strings.Join(textParts, "\n")}}
	}

	var msgs []domain.OpenAIMessage

	// Text + tool_calls in assistant message
	if len(textParts) > 0 || len(toolCalls) > 0 {
		msg := domain.OpenAIMessage{
			Role:      role,
			Content:   strings.Join(textParts, "\n"),
			ToolCalls: toolCalls,
		}
		msgs = append(msgs, msg)
	}

	// tool_result messages
	msgs = append(msgs, results...)

	return msgs
}

func extractToolResultContent(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

func translateTools(raw json.RawMessage) ([]domain.OpenAITool, error) {
	var anthropicTools []domain.AnthropicTool
	if err := json.Unmarshal(raw, &anthropicTools); err != nil {
		return nil, err
	}

	tools := make([]domain.OpenAITool, len(anthropicTools))
	for i, t := range anthropicTools {
		tools[i] = domain.OpenAITool{
			Type: "function",
			Function: domain.OpenAIFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}
	}
	return tools, nil
}

func translateToolChoice(raw json.RawMessage) any {
	var tc domain.AnthropicToolChoice
	if err := json.Unmarshal(raw, &tc); err != nil {
		return "auto"
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		return map[string]any{
			"type": "function",
			"function": map[string]any{"name": tc.Name},
		}
	default:
		return "auto"
	}
}

func extractSystemPrompt(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return joinTextBlocks(blocks)
	}
	return ""
}

func ExtractContentText(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return joinTextBlocks(blocks)
	}
	return string(raw)
}

func joinTextBlocks(blocks []domain.ContentBlock) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func translateStopSequences(seqs []string) any {
	if len(seqs) == 1 {
		return seqs[0]
	}
	return seqs
}
