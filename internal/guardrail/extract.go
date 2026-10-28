package guardrail

import (
	"encoding/json"
	"strings"
)

// --- structured extraction with per-field rewrite closures ---
//
// Each extractor parses the body into map[string]json.RawMessage layers
// (preserving unknown fields) and records, for every checkable text field, a
// segment: its byte range in the concatenated detection text plus a closure
// that writes a masked replacement back into the parsed structure. Mask spans
// returned by the service are applied per segment, so roles, arrays, media
// blocks and unknown fields survive masking untouched.

// maskApply mutates the parsed request so the field behind one segment holds
// the given masked text.
type maskApply func(masked string)

type contentSegment struct {
	start int
	end   int
	apply maskApply
}

// ExtractedContent is the protocol-aware view of one request body.
type ExtractedContent struct {
	Text   string
	segs   []contentSegment
	commit func() []byte
	maskOK *bool
	// Uninspectable marks bodies that carry non-text media parts alongside
	// checkable text (images, audio, files). Under strict coverage such
	// bodies are rejected instead of forwarding uninspectable content.
	Uninspectable bool
}

type contentBuilder struct {
	b             strings.Builder
	segs          []contentSegment
	maskOK        bool
	uninspectable bool
}

func newContentBuilder() *contentBuilder {
	return &contentBuilder{maskOK: true}
}

func (cb *contentBuilder) add(text string, apply maskApply) {
	if text == "" || apply == nil {
		return
	}
	if cb.b.Len() > 0 {
		cb.b.WriteString("\n")
	}
	start := cb.b.Len()
	cb.b.WriteString(text)
	cb.segs = append(cb.segs, contentSegment{start: start, end: start + len(text), apply: apply})
}

func (cb *contentBuilder) build(commit func() []byte) *ExtractedContent {
	if len(cb.segs) == 0 && !cb.uninspectable {
		return nil
	}
	return &ExtractedContent{Text: cb.b.String(), segs: cb.segs, commit: commit, maskOK: &cb.maskOK, Uninspectable: cb.uninspectable}
}

// ApplyMask patches every segment intersecting a span and returns the
// re-serialized body. allApplied=false when a tool-argument fragment could not
// be masked without breaking JSON validity.
func (e *ExtractedContent) ApplyMask(spans []Span) (body []byte, allApplied bool) {
	for _, seg := range e.segs {
		masked := maskSegment(e.Text, spans, seg.start, seg.end)
		if masked == nil {
			continue
		}
		seg.apply(*masked)
	}
	if e.commit != nil {
		body = e.commit()
	}
	return body, *e.maskOK
}

// maskSegment applies non-overlapping spans to the [start,end) fragment and
// returns the masked fragment, or nil when no span touches the range.
func maskSegment(content string, spans []Span, start, end int) *string {
	var b strings.Builder
	touched := false
	last := start
	for _, sp := range spans {
		if sp.End <= start || sp.Start >= end {
			continue
		}
		touched = true
		s := sp.Start
		if s < start {
			s = start
		}
		en := sp.End
		if en > end {
			en = end
		}
		if s > last {
			b.WriteString(content[last:s])
		}
		b.WriteString(sp.Replacement)
		last = en
	}
	if !touched {
		return nil
	}
	if last < end {
		b.WriteString(content[last:end])
	}
	out := b.String()
	return &out
}

// ExtractContent returns the protocol-aware checkable-text view of a gateway
// request body, dispatched on the request path. It returns nil when the path
// carries no checkable structured content.
func ExtractContent(body []byte, path string) *ExtractedContent {
	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return extractChatContent(body)
	case strings.HasSuffix(path, "/v1/messages"):
		return extractAnthropicContent(body)
	case strings.HasSuffix(path, "/v1/responses"):
		return extractResponsesContent(body)
	case strings.HasSuffix(path, "/v1/embeddings"):
		return extractEmbeddingsContent(body)
	case strings.HasSuffix(path, "/v1/images/generations"):
		return extractSimplePrompt(body)
	case strings.HasSuffix(path, "/v1/videos"):
		return extractSimplePrompt(body)
	case strings.HasSuffix(path, "/v1/audio/speech"):
		return extractSimplePrompt(body)
	case strings.HasPrefix(path, "/admin/api/playground/"):
		if strings.HasSuffix(path, "/chat") || strings.HasSuffix(path, "/stream") {
			return extractChatContent(body)
		}
		return extractSimplePrompt(body)
	}
	return nil
}

// RawString returns the value when raw is a JSON string.
func RawString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

// addTextBlockSegments adds one segment per text part of an OpenAI-style
// content value (string, or array of parts whose type is in textTypes —
// e.g. "text", or the Responses-native "input_text"/"output_text"). Media
// parts are neither checked nor touched.
func addTextBlockSegments(cb *contentBuilder, holder map[string]json.RawMessage, key string, textTypes ...string) {
	if len(textTypes) == 0 {
		textTypes = []string{"text"}
	}
	allowed := make(map[string]bool, len(textTypes))
	for _, t := range textTypes {
		allowed[t] = true
	}
	raw, ok := holder[key]
	if !ok {
		return
	}
	if s, isStr := RawString(raw); isStr {
		if s != "" {
			cb.add(s, func(masked string) {
				holder[key], _ = json.Marshal(masked)
			})
		}
		return
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return
	}
	for pi := range parts {
		t, ok := RawString(parts[pi]["type"])
		if !ok || !allowed[t] {
			if ok {
				// Non-text part (image, audio, file...): its bytes are not
				// inspectable by the text guardrail.
				cb.uninspectable = true
			}
			continue
		}
		text, ok := RawString(parts[pi]["text"])
		if !ok || text == "" {
			continue
		}
		cb.add(text, func(masked string) {
			parts[pi]["text"], _ = json.Marshal(masked)
			holder[key], _ = json.Marshal(parts)
		})
	}
}

// extractChatContent covers messages[].content (string or text parts),
// reasoning_content, assistant tool_calls[].function.arguments, and
// tools[].function.description.
func extractChatContent(body []byte) *ExtractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()

	var msgs []map[string]json.RawMessage
	hasMsgs := false
	if raw, ok := root["messages"]; ok {
		hasMsgs = json.Unmarshal(raw, &msgs) == nil
	}
	if hasMsgs {
		for i := range msgs {
			m := msgs[i]
			addTextBlockSegments(cb, m, "content")
			if s, ok := RawString(m["reasoning_content"]); ok && s != "" {
				cb.add(s, func(masked string) {
					m["reasoning_content"], _ = json.Marshal(masked)
				})
			}
			var tcs []map[string]json.RawMessage
			if raw, ok := m["tool_calls"]; ok && json.Unmarshal(raw, &tcs) == nil {
				for ti := range tcs {
					var fnMap map[string]json.RawMessage
					if json.Unmarshal(tcs[ti]["function"], &fnMap) != nil {
						continue
					}
					args, ok := RawString(fnMap["arguments"])
					if !ok || args == "" {
						continue
					}
					cb.add(canonicalJSONText(args), func(masked string) {
						if !json.Valid([]byte(masked)) {
							cb.maskOK = false
							return
						}
						fnMap["arguments"], _ = json.Marshal(masked)
						tcs[ti]["function"], _ = json.Marshal(fnMap)
						m["tool_calls"], _ = json.Marshal(tcs)
					})
				}
			}
		}
	}

	var tools []map[string]json.RawMessage
	if raw, ok := root["tools"]; ok && json.Unmarshal(raw, &tools) == nil {
		for ti := range tools {
			var fnMap map[string]json.RawMessage
			if json.Unmarshal(tools[ti]["function"], &fnMap) != nil {
				continue
			}
			desc, ok := RawString(fnMap["description"])
			if !ok || desc == "" {
				continue
			}
			cb.add(desc, func(masked string) {
				fnMap["description"], _ = json.Marshal(masked)
				tools[ti]["function"], _ = json.Marshal(fnMap)
				root["tools"], _ = json.Marshal(tools)
			})
		}
	}

	return cb.build(func() []byte {
		if hasMsgs {
			root["messages"], _ = json.Marshal(msgs)
		}
		out, _ := json.Marshal(root)
		return out
	})
}

// extractAnthropicContent covers system, message text blocks, tool_use.input
// and tool_result content (string or text blocks). Media blocks are untouched.
func extractAnthropicContent(body []byte) *ExtractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()

	addAnthropicBlocks := func(holder map[string]json.RawMessage, key string) {
		raw, ok := holder[key]
		if !ok {
			return
		}
		// writeBack re-serializes the parsed block array into the holder so
		// nested edits (e.g. tool_result text parts) reach the body.
		var blocks []map[string]json.RawMessage
		writeBack := func() {}
		if s, isStr := RawString(raw); isStr {
			if s != "" {
				cb.add(s, func(masked string) {
					holder[key], _ = json.Marshal(masked)
				})
			}
			return
		}
		if json.Unmarshal(raw, &blocks) != nil {
			return
		}
		writeBack = func() {
			holder[key], _ = json.Marshal(blocks)
		}
		for bi := range blocks {
			t, _ := RawString(blocks[bi]["type"])
			switch t {
			case "text":
				text, ok := RawString(blocks[bi]["text"])
				if !ok || text == "" {
					continue
				}
				cb.add(text, func(masked string) {
					blocks[bi]["text"], _ = json.Marshal(masked)
					writeBack()
				})
			case "tool_use", "tool_result":
				// handled below
			default:
				if t != "" {
					// image/document/etc blocks are not text-inspectable.
					cb.uninspectable = true
				}
			}
			if t == "tool_use" {
				input := blocks[bi]["input"]
				if len(input) == 0 {
					continue
				}
				cb.add(canonicalJSONText(string(input)), func(masked string) {
					if !json.Valid([]byte(masked)) {
						cb.maskOK = false
						return
					}
					blocks[bi]["input"] = json.RawMessage(masked)
					writeBack()
				})
			}
			if t == "tool_result" {
				rawC := blocks[bi]["content"]
				if s, isStr := RawString(rawC); isStr {
					if s == "" {
						continue
					}
					cb.add(s, func(masked string) {
						blocks[bi]["content"], _ = json.Marshal(masked)
						writeBack()
					})
					continue
				}
				var parts []map[string]json.RawMessage
				if json.Unmarshal(rawC, &parts) != nil {
					continue
				}
				for pi := range parts {
					pt, _ := RawString(parts[pi]["type"])
					if pt != "text" {
						if pt != "" {
							cb.uninspectable = true
						}
						continue
					}
					text, ok := RawString(parts[pi]["text"])
					if !ok || text == "" {
						continue
					}
					cb.add(text, func(masked string) {
						parts[pi]["text"], _ = json.Marshal(masked)
						blocks[bi]["content"], _ = json.Marshal(parts)
						writeBack()
					})
				}
			}
		}
	}

	if _, ok := root["system"]; ok {
		addAnthropicBlocks(root, "system")
	}

	var msgs []map[string]json.RawMessage
	hasMsgs := false
	if raw, ok := root["messages"]; ok {
		hasMsgs = json.Unmarshal(raw, &msgs) == nil
	}
	if hasMsgs {
		for i := range msgs {
			addAnthropicBlocks(msgs[i], "content")
		}
	}

	return cb.build(func() []byte {
		if hasMsgs {
			root["messages"], _ = json.Marshal(msgs)
		}
		out, _ := json.Marshal(root)
		return out
	})
}

// extractResponsesContent covers instructions, string input, and array input
// items: message content text, function_call.arguments and
// function_call_output.output.
func extractResponsesContent(body []byte) *ExtractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()

	if s, ok := RawString(root["instructions"]); ok && s != "" {
		cb.add(s, func(masked string) {
			root["instructions"], _ = json.Marshal(masked)
		})
	}

	var items []map[string]json.RawMessage
	hasItems := false
	if raw, ok := root["input"]; ok {
		if s, isStr := RawString(raw); isStr {
			if s != "" {
				cb.add(s, func(masked string) {
					root["input"], _ = json.Marshal(masked)
				})
			}
		} else {
			hasItems = json.Unmarshal(raw, &items) == nil
		}
	}
	if hasItems {
		for ii := range items {
			it := items[ii]
			t, _ := RawString(it["type"])
			switch t {
			case "message":
				// Responses-native content parts use input_text/output_text.
				addTextBlockSegments(cb, it, "content", "text", "input_text", "output_text")
			case "function_call":
				args, ok := RawString(it["arguments"])
				if !ok || args == "" {
					continue
				}
				cb.add(canonicalJSONText(args), func(masked string) {
					if !json.Valid([]byte(masked)) {
						cb.maskOK = false
						return
					}
					it["arguments"], _ = json.Marshal(masked)
				})
			case "function_call_output":
				out, ok := RawString(it["output"])
				if !ok || out == "" {
					continue
				}
				cb.add(out, func(masked string) {
					it["output"], _ = json.Marshal(masked)
				})
			}
		}
	}

	return cb.build(func() []byte {
		if hasItems {
			root["input"], _ = json.Marshal(items)
		}
		out, _ := json.Marshal(root)
		return out
	})
}

// extractEmbeddingsContent covers string input and string-array input.
func extractEmbeddingsContent(body []byte) *ExtractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()
	raw, ok := root["input"]
	if !ok {
		return nil
	}
	if s, isStr := RawString(raw); isStr {
		if s == "" {
			return nil
		}
		cb.add(s, func(masked string) {
			root["input"], _ = json.Marshal(masked)
		})
	} else {
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return nil
		}
		for i := range arr {
			s, ok := RawString(arr[i])
			if !ok {
				// Token-array input items are not text-inspectable.
				cb.uninspectable = true
				continue
			}
			if s == "" {
				continue
			}
			cb.add(s, func(masked string) {
				arr[i], _ = json.Marshal(masked)
				root["input"], _ = json.Marshal(arr)
			})
		}
	}
	return cb.build(func() []byte {
		out, _ := json.Marshal(root)
		return out
	})
}

// extractSimplePrompt covers flat {prompt|input} bodies (images, videos, TTS,
// simple playground calls).
func extractSimplePrompt(body []byte) *ExtractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()
	for _, key := range []string{"prompt", "input"} {
		if s, ok := RawString(root[key]); ok && s != "" {
			cb.add(s, func(masked string) {
				root[key], _ = json.Marshal(masked)
			})
			break
		}
	}
	return cb.build(func() []byte {
		out, _ := json.Marshal(root)
		return out
	})
}

// canonicalJSONText decodes and re-encodes a JSON document string so escaped
// sequences (\uXXXX) appear in DECODED form in the detection text — otherwise
// tool arguments carrying "SE\u0043RET" would never match a rule for
// "SECRET". Masked rewrites are written back in this canonical form.
func canonicalJSONText(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return raw
}
