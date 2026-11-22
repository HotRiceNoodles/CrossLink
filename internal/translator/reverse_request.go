package translator

import (
	"encoding/json"
	"log/slog"
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
		case "developer":
			// Current OpenAI spec: developer is the system-role alias.
			systemParts = append(systemParts, domain.ContentText(msg.Content))
		case "user":
			msgs = append(msgs, domain.AnthropicMessage{Role: "user", Content: reverseUserContent(msg.Content)})
		case "assistant":
			content, _ := json.Marshal(assistantToContentBlocks(msg))
			msgs = append(msgs, domain.AnthropicMessage{Role: "assistant", Content: content})
		case "tool", "function":
			// "function" is the legacy OpenAI equivalent of a tool result message.
			content, _ := json.Marshal(toolResultToContentBlocks(msg))
			msgs = append(msgs, domain.AnthropicMessage{Role: "user", Content: content})
		default:
			slog.Warn("reverse translator: dropping message with unknown role",
				"role", msg.Role)
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
		if parts, ok := msg.Content.([]any); ok {
			// Part-array tool content: map parts onto Anthropic blocks — a
			// verbatim passthrough would leak OpenAI-only shapes (image_url)
			// into tool_result content and 400 the upstream.
			raw, _ := json.Marshal(reverseContentParts(parts))
			block.Content = raw
		} else {
			raw, _ := json.Marshal(msg.Content)
			block.Content = raw
		}
	}
	return []domain.ContentBlock{block}
}

// reverseUserContent converts an OpenAI user-message content (plain string or
// array of content parts) into Anthropic message content. String or nil
// content passes through verbatim; part arrays go through
// reverseContentParts.
func reverseUserContent(content any) json.RawMessage {
	parts, ok := content.([]any)
	if !ok {
		raw, _ := json.Marshal(content)
		return raw
	}
	raw, _ := json.Marshal(reverseContentParts(parts))
	return raw
}

// reverseContentParts maps OpenAI content parts onto Anthropic blocks:
// text parts pass through verbatim (the two shapes coincide), image_url parts
// are converted to image blocks, and part types with no Anthropic equivalent
// are dropped with a warning — passing them through verbatim is what produced
// upstream 400s like "unsupported content type 'image_url'".
func reverseContentParts(parts []any) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		m, ok := part.(map[string]any)
		if !ok {
			slog.Warn("reverse translator: dropping non-object content part")
			continue
		}
		switch m["type"] {
		case "text":
			raw, _ := json.Marshal(m)
			out = append(out, raw)
		case "image_url":
			if block, ok := imageURLPartToBlock(m); ok {
				raw, _ := json.Marshal(block)
				out = append(out, raw)
			}
		default:
			slog.Warn("reverse translator: dropping content part with no Anthropic equivalent",
				"part_type", m["type"])
		}
	}
	if len(out) == 0 {
		// Anthropic rejects empty content arrays; keep an empty text block so
		// the message stays present.
		out = append(out, json.RawMessage(`{"type":"text","text":""}`))
	}
	return out
}

// imageURLPartToBlock converts an OpenAI image_url content part into an
// Anthropic image block. Base64 data URLs become {"type":"base64",...}
// sources; http(s) URLs become {"type":"url",...} sources.
func imageURLPartToBlock(part map[string]any) (domain.ContentBlock, bool) {
	url := ""
	if iu, ok := part["image_url"].(map[string]any); ok {
		url, _ = iu["url"].(string)
	}
	if url == "" {
		slog.Warn("reverse translator: dropping image_url part without url")
		return domain.ContentBlock{}, false
	}
	if mediaType, data, ok := parseDataURL(url); ok {
		source, _ := json.Marshal(map[string]string{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		})
		return domain.ContentBlock{Type: "image", Source: source}, true
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		source, _ := json.Marshal(map[string]string{"type": "url", "url": url})
		return domain.ContentBlock{Type: "image", Source: source}, true
	}
	// Never log the raw url — data URLs embed the full image payload.
	slog.Warn("reverse translator: dropping image_url part with unsupported url form")
	return domain.ContentBlock{}, false
}

// parseDataURL splits a "data:<media_type>;base64,<payload>" URL.
func parseDataURL(url string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return "", "", false
	}
	meta := url[len(prefix):comma]
	const base64Suffix = ";base64"
	if !strings.HasSuffix(meta, base64Suffix) {
		return "", "", false
	}
	return strings.TrimSuffix(meta, base64Suffix), url[comma+1:], true
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
