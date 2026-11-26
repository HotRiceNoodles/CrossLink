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
			msgs = append(msgs, domain.AnthropicMessage{Role: "user", Content: reverseUserContent(msg.Content, msg.CacheMarker)})
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
		// Prompt-cache carry: an anthropic-inbound system block array carries
		// cache_control inside its raw bytes — re-emit the ORIGINAL verbatim
		// for same-protocol upstreams; the joined-string fallback covers the
		// openai-inbound (no markers possible) path.
		if req.InboundCacheMeta != nil && len(req.InboundCacheMeta.SystemRaw) > 0 {
			anthropicReq.System = req.InboundCacheMeta.SystemRaw
		} else {
			sys, _ := json.Marshal(strings.Join(systemParts, "\n"))
			anthropicReq.System = sys
		}
	}

	if len(req.Tools) > 0 {
		// Prompt-cache carry: per-tool cache_control markers ride in the
		// original tools bytes; re-emit verbatim (the structured rebuild below
		// cannot express them). The rebuild stays for openai-inbound tools.
		if req.InboundCacheMeta != nil && len(req.InboundCacheMeta.ToolsRaw) > 0 {
			anthropicReq.Tools = req.InboundCacheMeta.ToolsRaw
		} else {
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
	// Phase-2 compact 前置: carried unrecognized-typed blocks (Raw bytes —
	// e.g. compaction replay) re-emit VERBATIM after the rebuilt blocks in
	// both paths below; dropping them would break the upstream's
	// replace-compacted-history protocol on the next turn.
	appendUnknown := func(blocks []domain.ContentBlock) []domain.ContentBlock {
		return append(blocks, msg.UnknownBlocks...)
	}
	// 类二 M-A: a carried thinking-block sequence replays VERBATIM (values
	// incl. signature/data) ahead of the rebuilt text/tool blocks — the
	// model's own reasoning must return byte-for-byte in value terms, or the
	// upstream rejects/loses continuity.
	if len(msg.ThinkingBlocks) > 0 {
		blocks := make([]domain.ContentBlock, 0, len(msg.ThinkingBlocks)+len(msg.UnknownBlocks)+2)
		blocks = append(blocks, msg.ThinkingBlocks...)
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
		if len(blocks) == len(msg.ThinkingBlocks) && len(msg.UnknownBlocks) == 0 {
			blocks = append(blocks, domain.ContentBlock{Type: "text", Text: ""})
		}
		return appendUnknown(blocks)
	}
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
	blocks = appendUnknown(blocks)
	if len(blocks) == 0 {
		blocks = append(blocks, domain.ContentBlock{Type: "text", Text: ""})
	}
	// Prompt-cache carry: re-attach the inbound marker onto the LAST block
	// (the boundary position Anthropic's own guidance uses).
	if len(msg.CacheMarker) > 0 {
		blocks[len(blocks)-1].CacheControl = msg.CacheMarker
	}
	return blocks
}

func toolResultToContentBlocks(msg domain.OpenAIMessage) []domain.ContentBlock {
	block := domain.ContentBlock{
		Type:         "tool_result",
		ToolUseID:    msg.ToolCallID,
		CacheControl: msg.CacheMarker,
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
// reverseContentParts. A carried prompt-cache marker upgrades a string into a
// single-block array (a bare string cannot carry cache_control) or is
// injected into the last rebuilt part.
func reverseUserContent(content any, marker json.RawMessage) json.RawMessage {
	parts, ok := content.([]any)
	if !ok {
		if len(marker) == 0 {
			raw, _ := json.Marshal(content)
			return raw
		}
		text, _ := content.(string)
		blocks := []domain.ContentBlock{{Type: "text", Text: text, CacheControl: marker}}
		raw, _ := json.Marshal(blocks)
		return raw
	}
	out := reverseContentParts(parts)
	if len(marker) > 0 && len(out) > 0 {
		out[len(out)-1] = withCacheMarker(out[len(out)-1], marker)
	}
	raw, _ := json.Marshal(out)
	return raw
}

// withCacheMarker injects a cache_control marker into a raw content-part
// block, preserving all other fields.
func withCacheMarker(raw json.RawMessage, marker json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	m["cache_control"] = marker
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
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
