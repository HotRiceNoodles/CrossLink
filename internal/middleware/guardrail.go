package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

const maxGuardrailBodySize = 10 << 20

func GuardrailsRequest(svc *guardrail.GuardrailService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || !svc.IsEnabled() {
			c.Next()
			return
		}

		// Multipart file-upload bodies (audio transcription/translation, image
		// edits) carry binary media the text guardrail cannot inspect — and
		// MUST NOT be buffered here: a truncated (maxGuardrailBodySize) copy
		// replacing the original body corrupts the multipart framing for
		// uploads larger than that (body.go exempts the same path set from
		// ReadBody for exactly this reason). Under strict coverage they are
		// rejected below; otherwise they pass through (documented gap).
		multipartFile := strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") &&
			(isFileUploadPath(c.Request.URL.Path) ||
				strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground/transcribe") ||
				strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground/translate"))

		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Skip admin routes (except playground which proxies LLM calls)
		if strings.HasPrefix(c.Request.URL.Path, "/admin/api/") && !strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground") {
			c.Next()
			return
		}

		if multipartFile && !svc.StrictCoverage() {
			c.Next()
			return
		}

		bodyBytes := GetBodyBytes(c)
		if bodyBytes == nil {
			var err error
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, int64(maxGuardrailBodySize)))
			if err != nil {
				slog.Error("guardrail: failed to read request body", "error", err)
				c.JSON(http.StatusBadRequest, gin.H{
					"error": gin.H{"type": "guardrail_error", "message": "failed to read request body"},
				})
				c.Abort()
				return
			}
			c.Request.Body.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(bodyBytes, &probe) != nil && !multipartFile {
			c.Next()
			return
		}

		ex := extractContent(bodyBytes, c.Request.URL.Path)
		streamContent := ""
		if ex != nil {
			streamContent = ex.text
		}
		piiCtx, piiVault, piiErr := guardrail.WithPIIVault(c.Request.Context())
		if piiErr != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "guardrail_error", "message": "PII mapping unavailable"}})
			return
		}
		c.Request = c.Request.WithContext(piiCtx)
		defer piiVault.Clear()
		next := func() {
			if c.IsAborted() {
				return
			}
			if piiVault.Active() {
				if c.GetHeader("X-Idempotency-Key") != "" {
					c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "reversible PII masking does not support X-Idempotency-Key; mappings are request-scoped"}})
					return
				}
				finish := installPIIRestore(c, piiVault, svc.StrictMaxBytes(), svc.StrictMaxEvents())
				defer finish()
			}
			// Response engines are opened only after request-side approval;
			// contextual engines receive the transformed request when masked.
			if probe.Stream && svc.StreamMode() == "incremental" {
				meta := guardrail.CheckRequest{Content: streamContent, Model: probe.Model, Direction: guardrail.DirectionResponse, OrgID: c.GetInt64("org_id")}
				if key := GetAPIKeyFromContext(c); key != nil {
					meta.APIKeyID = key.ID
					if key.TeamID != nil {
						meta.TeamID = *key.TeamID
					}
				}
				finish, err := installIncrementalStream(c, svc, meta)
				if err != nil {
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "guardrail_error", "message": "stream guardrail unavailable"}})
					return
				}
				defer finish()
			}
			c.Next()
		}

		// Strict coverage: a POST gateway content endpoint whose body we cannot
		// structurally inspect — no extractable text, or non-text media parts
		// riding alongside it — must be rejected, never silently passed.
		if svc.StrictCoverage() && isGatewayContentPath(c.Request.URL.Path) &&
			(ex == nil || ex.uninspectable) {
			reason := "endpoint content cannot be inspected by the guardrail under strict coverage policy"
			switch {
			case multipartFile:
				reason = "multipart media content cannot be inspected by the text guardrail under strict coverage policy"
			case ex != nil && ex.uninspectable:
				reason = "request contains non-text media parts that cannot be inspected by the text guardrail under strict coverage policy"
			}
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{"type": "guardrail_uncovered", "message": reason},
			})
			c.Abort()
			return
		}
		if ex == nil || ex.text == "" {
			next()
			return
		}

		var apiKeyID, teamID int64
		if key := GetAPIKeyFromContext(c); key != nil {
			apiKeyID = key.ID
			if key.TeamID != nil {
				teamID = *key.TeamID
			}
		}

		var orgID int64
		if v := c.GetInt64("org_id"); v != 0 {
			orgID = v
		}

		checkCtx := &guardrail.CheckContext{Metadata: make(map[string]string)}
		if apiKeyID > 0 {
			checkCtx.Metadata["api_key_id"] = fmt.Sprintf("%d", apiKeyID)
		}
		ctx := context.WithValue(c.Request.Context(), guardrail.CtxKeyCheck, checkCtx)
		ctx = context.WithValue(ctx, guardrail.CtxKeyUserAgent, c.GetHeader("User-Agent"))
		ctx = context.WithValue(ctx, guardrail.CtxKeyHeaders, c.Request.Header)
		c.Request = c.Request.WithContext(ctx)

		guardCtx, cancel := context.WithTimeout(ctx, time.Duration(svc.RequestBudgetMs())*time.Millisecond)
		defer cancel()

		result, err := svc.Check(guardCtx, &guardrail.CheckRequest{
			Content:   ex.text,
			Direction: guardrail.DirectionRequest,
			Model:     probe.Model,
			APIKeyID:  apiKeyID,
			TeamID:    teamID,
			OrgID:     orgID,
		})
		if err != nil {
			if svc.IsFailOpen() {
				slog.Warn("guardrail check failed, failing open", "error", err)
				next()
				return
			}
			slog.Error("guardrail: request check failed", "error", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"type": "guardrail_error", "message": "guardrail service unavailable"},
			})
			c.Abort()
			return
		}

		// Extract agent_type from CheckContext for usage logging
		if checkCtx.AgentType != "" {
			c.Set("agent_type", checkCtx.AgentType)
		}

		// Partial coverage: the checked content was over the inspection limit
		// and only its prefix was scanned. Surface it — never a silent pass.
		if result != nil && result.Truncated {
			c.Header("X-Guardrail-Coverage", "partial")
			c.Set("guardrail_truncated", true)
			setSecurityEvent(c, result, "partial coverage: content exceeded inspection limit; only the prefix was scanned")
		}

		if result == nil || !result.Blocked {
			if result != nil && (result.Action == "log" || result.Action == "mask") {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", result.RuleName)
				setSecurityEvent(c, result, "")
				if result.Action == "mask" {
					applyRequestMask(c, ex, result)
					streamContent = result.MaskedContent
				}
			}
			next()
			return
		}

		switch result.Action {
		case "block":
			writeGuardrailError(c, result, c.Request.URL.Path)
			c.Abort()
			return
		case "review":
			// 拦截待人工复核：内容不放行，复核结论只完成审计闭环。
			writeGuardrailError(c, result, c.Request.URL.Path)
			c.Abort()
			return
		case "log":
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
			setSecurityEvent(c, result, "")
		case "mask":
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
			setSecurityEvent(c, result, "")
			applyRequestMask(c, ex, result)
			streamContent = result.MaskedContent
		}

		next()
	}
}

// applyRequestMask rewrites the request body with the composed mask spans,
// field by field, preserving roles, arrays and non-text (media) blocks.
// Engines that return only a whole-string MaskedContent (no spans) are NOT
// written back: collapsing all user messages into one masked string destroys
// structure. If a mask cannot be applied completely, reject the request.
func applyRequestMask(c *gin.Context, ex *extractedContent, result *guardrail.CheckResponse) {
	if len(result.MaskedSpans) == 0 {
		slog.Warn("guardrail: mask result without field spans; rejecting request",
			"rule", result.RuleName)
		setSecurityEvent(c, result, "mask not applied: engine returned no field spans")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail mask could not be applied safely"}})
		return
	}
	newBody, allApplied := ex.applyMask(result.MaskedSpans)
	if !allApplied || newBody == nil {
		setSecurityEvent(c, result, "mask could not be applied safely; request rejected")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail mask could not be applied safely"}})
		return
	}
	// Rewrite BOTH the request body and the buffered copy: downstream consumers
	// (cache key, handlers, usage logging) read via GetBodyBytes, which without
	// this sync would keep serving the unmasked original to the upstream.
	// The rewrite is budget-aware: a masked body larger than the original must
	// stay inside the per-request ceiling and the in-flight budget (supervisor
	// F26 review: rewrites may not silently widen memory beyond admission).
	if !SetBodyBytes(c, newBody) {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"type": "request_too_large", "message": "masked request body exceeds the body-size ceiling"},
		})
		return
	}
}

func isGatewayContentPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/admin/api/playground/")
}

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

// extractedContent is the protocol-aware view of one request body.
type extractedContent struct {
	text   string
	segs   []contentSegment
	commit func() []byte
	maskOK *bool
	// uninspectable marks bodies that carry non-text media parts alongside
	// checkable text (images, audio, files). Under strict coverage such
	// bodies are rejected instead of forwarding uninspectable content.
	uninspectable bool
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

func (cb *contentBuilder) build(commit func() []byte) *extractedContent {
	if len(cb.segs) == 0 && !cb.uninspectable {
		return nil
	}
	return &extractedContent{text: cb.b.String(), segs: cb.segs, commit: commit, maskOK: &cb.maskOK, uninspectable: cb.uninspectable}
}

// applyMask patches every segment intersecting a span and returns the
// re-serialized body. allApplied=false when a tool-argument fragment could not
// be masked without breaking JSON validity.
func (e *extractedContent) applyMask(spans []guardrail.Span) (body []byte, allApplied bool) {
	for _, seg := range e.segs {
		masked := maskSegment(e.text, spans, seg.start, seg.end)
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
func maskSegment(content string, spans []guardrail.Span, start, end int) *string {
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

func extractContent(body []byte, path string) *extractedContent {
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

// rawString returns the value when raw is a JSON string.
func rawString(raw json.RawMessage) (string, bool) {
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
	if s, isStr := rawString(raw); isStr {
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
		t, ok := rawString(parts[pi]["type"])
		if !ok || !allowed[t] {
			if ok {
				// Non-text part (image, audio, file...): its bytes are not
				// inspectable by the text guardrail.
				cb.uninspectable = true
			}
			continue
		}
		text, ok := rawString(parts[pi]["text"])
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
func extractChatContent(body []byte) *extractedContent {
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
			if s, ok := rawString(m["reasoning_content"]); ok && s != "" {
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
					args, ok := rawString(fnMap["arguments"])
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
			desc, ok := rawString(fnMap["description"])
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
func extractAnthropicContent(body []byte) *extractedContent {
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
		if s, isStr := rawString(raw); isStr {
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
			t, _ := rawString(blocks[bi]["type"])
			switch t {
			case "text":
				text, ok := rawString(blocks[bi]["text"])
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
				if s, isStr := rawString(rawC); isStr {
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
					pt, _ := rawString(parts[pi]["type"])
					if pt != "text" {
						if pt != "" {
							cb.uninspectable = true
						}
						continue
					}
					text, ok := rawString(parts[pi]["text"])
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
func extractResponsesContent(body []byte) *extractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()

	if s, ok := rawString(root["instructions"]); ok && s != "" {
		cb.add(s, func(masked string) {
			root["instructions"], _ = json.Marshal(masked)
		})
	}

	var items []map[string]json.RawMessage
	hasItems := false
	if raw, ok := root["input"]; ok {
		if s, isStr := rawString(raw); isStr {
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
			t, _ := rawString(it["type"])
			switch t {
			case "message":
				// Responses-native content parts use input_text/output_text.
				addTextBlockSegments(cb, it, "content", "text", "input_text", "output_text")
			case "function_call":
				args, ok := rawString(it["arguments"])
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
				out, ok := rawString(it["output"])
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
func extractEmbeddingsContent(body []byte) *extractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()
	raw, ok := root["input"]
	if !ok {
		return nil
	}
	if s, isStr := rawString(raw); isStr {
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
			s, ok := rawString(arr[i])
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
func extractSimplePrompt(body []byte) *extractedContent {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	cb := newContentBuilder()
	for _, key := range []string{"prompt", "input"} {
		if s, ok := rawString(root[key]); ok && s != "" {
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

func setSecurityEvent(c *gin.Context, result *guardrail.CheckResponse, note string) {
	event := map[string]string{
		"rule":   result.RuleName,
		"action": result.Action,
		"reason": result.Reason,
	}
	if note != "" {
		event["note"] = note
	}
	if result.Truncated {
		event["truncated"] = "true"
	}
	if data, err := json.Marshal([]map[string]string{event}); err == nil {
		c.Set("security_events", datatypes.JSON(data))
	}
}

func writeGuardrailError(c *gin.Context, result *guardrail.CheckResponse, path string) {
	msg := "blocked by guardrail"

	if strings.HasSuffix(path, "/v1/messages") {
		c.JSON(http.StatusBadRequest, gin.H{
			"type":  "error",
			"error": gin.H{"type": "guardrail_blocked", "message": msg},
		})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{"type": "guardrail_blocked", "message": msg},
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
