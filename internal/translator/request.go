package translator

import (
	"encoding/json"
	"errors"
	"log/slog"
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
		// M3 envelope carry (design §1.1): the translator is envelope-blind —
		// one verbatim assignment, no semantic handling.
		Extensions: req.Extensions,
	}
	if req.TopK != nil {
		// top_k has no equivalent field in the internal OpenAI representation;
		// carry it as metadata so an Anthropic-protocol upstream can enforce
		// its version rules at the outbound checkpoint (M0). Old-version
		// upstreams keep the existing drop-on-translate behavior — this carry
		// does not restore top_k for them, it only prevents it from bypassing
		// the 2026-10-21 rejection rule.
		oaiReq.InboundSamplingMeta = &domain.InboundSamplingMeta{TopK: req.TopK}
	}
	// Prompt-cache carry (system/tools half): a block-array system and the
	// tools array carry cache_control markers INSIDE their raw bytes — no
	// structured rebuild preserves them. Capture the originals verbatim; the
	// reverse translation re-emits them for same-protocol upstreams. A plain
	// string system cannot carry markers and needs no capture.
	if isBlockArrayJSON(req.System) || len(req.Tools) > 0 {
		oaiReq.InboundCacheMeta = &domain.InboundCacheMeta{
			SystemRaw: blockArraySystem(req.System),
			ToolsRaw:  req.Tools,
		}
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

	out := translateContentBlocks(msg.Role, blocks)
	// 类二 M-A: assistant thinking/redacted blocks have no OpenAI equivalent —
	// carry their VALUES on the translated assistant message so the reverse
	// translation can re-emit them (signature intact) for same-protocol
	// upstreams. Only assistant messages legally contain thinking blocks.
	if msg.Role == "assistant" {
		for i := range out {
			if out[i].Role == "assistant" && len(blocks) > 0 && hasThinkingBlocks(blocks) {
				out[i].ThinkingBlocks = collectThinkingBlocks(blocks)
				break
			}
		}
	}
	return out
}

func hasThinkingBlocks(blocks []domain.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "thinking" || b.Type == "redacted_thinking" {
			return true
		}
	}
	return false
}

func collectThinkingBlocks(blocks []domain.ContentBlock) []domain.ContentBlock {
	var out []domain.ContentBlock
	for _, b := range blocks {
		if b.Type == "thinking" || b.Type == "redacted_thinking" {
			out = append(out, b)
		}
	}
	return out
}

func translateContentBlocks(role string, blocks []domain.ContentBlock) []domain.OpenAIMessage {
	// Collect text blocks and tool_use/tool_result blocks separately
	var textParts []string
	var toolCalls []domain.OpenAIToolCall
	var results []domain.OpenAIMessage
	var imageParts []any

	// Prompt-cache marker capture (per-message half of the carry): the marker
	// of the last marked block contributing to each emitted message rides on
	// that message's CacheMarker; the reverse translation re-attaches it to
	// the last rebuilt block. Joined-text buckets keep the LAST marker seen
	// (best effort: the boundary lands at message end, which is at or after
	// the original position — cache prefixes only grow, never shrink).
	var textMarker, assistantMarker, imageMarker json.RawMessage

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
			if len(b.CacheControl) > 0 {
				textMarker = b.CacheControl
			}
		case "tool_use":
			toolCalls = append(toolCalls, domain.OpenAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: domain.OpenAIFunctionCall{
					Name:      b.Name,
					Arguments: string(b.Input),
				},
			})
			if len(b.CacheControl) > 0 {
				assistantMarker = b.CacheControl
			}
		case "tool_result":
			content := extractToolResultContent(b.Content)
			m := domain.OpenAIMessage{
				Role:        "tool",
				Content:     content,
				ToolCallID:  b.ToolUseID,
				CacheMarker: b.CacheControl,
			}
			results = append(results, m)
		case "image":
			if part, ok := imageBlockToPart(b); ok {
				imageParts = append(imageParts, part)
				if len(b.CacheControl) > 0 {
					imageMarker = b.CacheControl
				}
			}
		}
	}

	// If only text blocks, return a simple message
	if len(toolCalls) == 0 && len(results) == 0 {
		if len(imageParts) == 0 {
			return []domain.OpenAIMessage{{Role: role, Content: strings.Join(textParts, "\n"), CacheMarker: textMarker}}
		}
		content := make([]any, 0, len(textParts)+len(imageParts))
		if len(textParts) > 0 {
			content = append(content, map[string]any{"type": "text", "text": strings.Join(textParts, "\n")})
		}
		content = append(content, imageParts...)
		return []domain.OpenAIMessage{{Role: role, Content: content, CacheMarker: lastMarker(textMarker, imageMarker)}}
	}

	var msgs []domain.OpenAIMessage

	// Text + tool_calls in assistant message
	if len(textParts) > 0 || len(toolCalls) > 0 {
		msg := domain.OpenAIMessage{
			Role:        role,
			Content:     strings.Join(textParts, "\n"),
			ToolCalls:   toolCalls,
			CacheMarker: lastMarker(textMarker, assistantMarker),
		}
		msgs = append(msgs, msg)
	}

	// tool_result messages
	msgs = append(msgs, results...)

	// Images cannot ride along a tool-role message in OpenAI shape; emit them
	// as a trailing message so they are not silently dropped.
	if len(imageParts) > 0 {
		msgs = append(msgs, domain.OpenAIMessage{Role: role, Content: imageParts, CacheMarker: imageMarker})
	}

	return msgs
}

// lastMarker returns the later of two captured prompt-cache markers (the
// second wins when both exist — later block = later boundary).
func lastMarker(a, b json.RawMessage) json.RawMessage {
	if len(b) > 0 {
		return b
	}
	return a
}

// isBlockArrayJSON reports whether raw is a JSON array (the anthropic system
// field's block-array form — the only form that can carry cache_control).
func isBlockArrayJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

// blockArraySystem returns the original system bytes when they are a block
// array (markers preserved verbatim), nil otherwise.
func blockArraySystem(raw json.RawMessage) json.RawMessage {
	if isBlockArrayJSON(raw) {
		return raw
	}
	return nil
}

// imageBlockToPart converts an Anthropic image block into an OpenAI
// {"type":"image_url","image_url":{"url":...}} content part. Base64 sources
// become data: URLs; url sources pass through. Blocks with unusable sources
// are dropped with a warning.
func imageBlockToPart(b domain.ContentBlock) (map[string]any, bool) {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(b.Source, &source); err != nil {
		slog.Warn("translator: dropping image block with invalid source JSON")
		return nil, false
	}
	switch source.Type {
	case "base64":
		if source.MediaType == "" || source.Data == "" {
			slog.Warn("translator: dropping base64 image block missing media_type or data")
			return nil, false
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + source.MediaType + ";base64," + source.Data},
		}, true
	case "url":
		if source.URL == "" {
			slog.Warn("translator: dropping url image block missing url")
			return nil, false
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": source.URL},
		}, true
	}
	slog.Warn("translator: dropping image block with unsupported source type", "source_type", source.Type)
	return nil, false
}

// extractToolResultContent converts Anthropic tool_result content into an
// OpenAI tool-message content: a plain string when text-only (the common
// case, byte-identical to what an OpenAI client sends), or a parts array
// when the result carries image blocks.
func extractToolResultContent(raw json.RawMessage) any {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var texts []string
	var imageParts []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			if part, ok := imageBlockToPart(b); ok {
				imageParts = append(imageParts, part)
			}
		}
	}
	if len(imageParts) == 0 {
		return strings.Join(texts, "\n")
	}
	content := make([]any, 0, len(texts)+len(imageParts))
	if len(texts) > 0 {
		content = append(content, map[string]any{"type": "text", "text": strings.Join(texts, "\n")})
	}
	content = append(content, imageParts...)
	return content
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
			"type":     "function",
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
