package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

// Content guardrail integration for the MCP gateway.
//
// Tool-permission checks, authentication, rate limits and SSRF protections are
// unchanged; this layer adds content inspection at the two points where text
// actually crosses the gateway:
//
//   - before ForwardRequest: the tools/call arguments,
//   - after ForwardRequest: the textual parts of the tool result.
//
// Blocking answers use a JSON-RPC error so both the HTTP and SSE transports
// surface the same contract. Masking rewrites the specific field in place and
// requires the rewritten arguments to stay valid JSON — otherwise the call is
// rejected (controlled refusal) rather than forwarded unmasked.

// mcpGuardrailError codes the guardrail rejection as a JSON-RPC application error.
const mcpGuardrailErrorCode = -32000

func (h *Handler) SetGuardrailSvc(svc *guardrail.GuardrailService) {
	h.guard = svc
}

func mcpIdentity(c *gin.Context) (apiKeyID, teamID, orgID int64) {
	if v, ok := c.Get("api_key_id"); ok {
		if id, ok := v.(int64); ok {
			apiKeyID = id
		}
	}
	if v, ok := c.Get("api_key"); ok {
		if key, ok := v.(*model.APIKey); ok && key != nil {
			if key.TeamID != nil {
				teamID = *key.TeamID
			}
			if key.OrgID != nil {
				orgID = *key.OrgID
			}
		}
	}
	return apiKeyID, teamID, orgID
}

// guardrailActive reports whether content checks apply.
func (h *Handler) guardrailActive() bool {
	return h.guard != nil && h.guard.IsEnabled()
}

// checkToolCallParams inspects tools/call params (name + arguments) before the
// call is forwarded. Params are canonicalized (decode + re-encode) first so
// JSON-escaped payloads (\uXXXX) are checked in decoded form. It returns a
// JSON-RPC error to return to the client when the call must not proceed, and
// may rewrite req.Params in place for mask actions.
func (h *Handler) checkToolCallParams(c *gin.Context, req *JSONRPCRequest) *JSONRPCError {
	if !h.guardrailActive() || req.Method != MethodToolsCall || len(req.Params) == 0 {
		return nil
	}
	apiKeyID, teamID, orgID := mcpIdentity(c)

	canonical := canonicalJSON(req.Params)

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(h.guard.RequestBudgetMs())*time.Millisecond)
	defer cancel()
	result, err := h.guard.Check(ctx, &guardrail.CheckRequest{
		Content:   string(canonical),
		Direction: guardrail.DirectionRequest,
		APIKeyID:  apiKeyID,
		TeamID:    teamID,
		OrgID:     orgID,
	})
	if err != nil {
		if h.guard.IsFailOpen() {
			return nil
		}
		return &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "guardrail unavailable"}
	}
	if result == nil || !result.Blocked {
		if result != nil && result.Action == "mask" {
			if ok := applyMaskToRawJSON(&canonical, result); !ok {
				return &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "blocked by guardrail (mask could not be applied safely)"}
			}
			req.Params = canonical
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
		}
		return nil
	}
	c.Set("guardrail_triggered", true)
	c.Set("guardrail_rule", result.RuleName)
	return &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "blocked by guardrail"}
}

// canonicalJSON decodes and re-encodes a JSON document so escaped sequences
// (\uXXXX) are checked in their decoded form; on any error the original bytes
// are returned unchanged.
func canonicalJSON(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	if b, err := json.Marshal(v); err == nil {
		return b
	}
	return raw
}

// checkToolCallResult inspects the textual parts of a forwarded tool result
// before they are returned to the client. It may rewrite resp.Result in place
// for mask actions; a blocking result is replaced by a JSON-RPC error.
func (h *Handler) checkToolCallResult(c *gin.Context, req *JSONRPCRequest, resp *JSONRPCResponse) {
	if !h.guardrailActive() || resp == nil || resp.Result == nil || resp.Error != nil {
		return
	}
	// Only tools/call results carry tool content.
	if req.Method != MethodToolsCall {
		return
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return
	}
	// Strict coverage: results carrying content shapes the text guardrail
	// cannot inspect (resource links, images, audio, embedded blobs) are
	// rejected explicitly instead of passing as "no text found".
	if h.guard.StrictCoverage() && mcpResultHasUninspectableContent(raw) {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", "guardrail_uncovered")
		resp.Result = nil
		resp.Error = &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "tool result contains content types the guardrail cannot inspect under strict coverage policy"}
		return
	}
	text, ok := mcpResultText(raw)
	if !ok || text == "" {
		return
	}
	apiKeyID, teamID, orgID := mcpIdentity(c)

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(h.guard.ResponseBudgetMs())*time.Millisecond)
	defer cancel()
	result, err := h.guard.Check(ctx, &guardrail.CheckRequest{
		Content:   text,
		Direction: guardrail.DirectionResponse,
		APIKeyID:  apiKeyID,
		TeamID:    teamID,
		OrgID:     orgID,
	})
	if err != nil {
		if h.guard.IsFailOpen() {
			return
		}
		resp.Result = nil
		resp.Error = &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "guardrail unavailable"}
		return
	}
	if result == nil {
		return
	}
	if result.Action == "mask" && len(result.MaskedSpans) > 0 {
		if masked, ok := maskMCPResultText(raw, text, result.MaskedSpans); ok {
			var rewritten json.RawMessage = masked
			if json.Unmarshal(rewritten, &resp.Result) == nil {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", result.RuleName)
				return
			}
		}
		// Could not rewrite safely — refuse the content (controlled).
		resp.Result = nil
		resp.Error = &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "blocked by guardrail (mask could not be applied safely)"}
		return
	}
	if result.Blocked {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", result.RuleName)
		resp.Result = nil
		resp.Error = &JSONRPCError{Code: mcpGuardrailErrorCode, Message: "blocked by guardrail"}
	}
}

// mcpResultText extracts the textual parts of a tools/call result body —
// content[].text AND the serialized structuredContent object — and reports
// whether the body had the expected shape.
func mcpResultText(raw []byte) (string, bool) {
	var body struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", false
	}
	var parts []string
	for _, p := range body.Content {
		if p.Type == "text" && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	if len(body.StructuredContent) > 0 {
		parts = append(parts, string(canonicalJSON(body.StructuredContent)))
	}
	return strings.Join(parts, "\n"), true
}

// maskMCPResultText applies spans to content[].text parts and to the
// structuredContent serialization in place.
func maskMCPResultText(raw []byte, joined string, spans []guardrail.Span) ([]byte, bool) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return nil, false
	}
	offset := 0
	changed := false
	if contentRaw, ok := body["content"]; ok {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(contentRaw, &parts) == nil {
			for pi := range parts {
				t, _ := jsonToString(parts[pi]["type"])
				text, ok := jsonToString(parts[pi]["text"])
				if !ok || t != "text" || text == "" {
					continue
				}
				masked := maskTextRange(joined, spans, offset, offset+len(text))
				offset += len(text) + 1 // separator
				if masked == nil {
					continue
				}
				parts[pi]["text"], _ = json.Marshal(*masked)
				changed = true
			}
			if changed {
				body["content"], _ = json.Marshal(parts)
			}
		}
	}
	if scRaw, ok := body["structuredContent"]; ok && len(scRaw) > 0 {
		canonical := canonicalJSON(scRaw)
		masked := maskTextRange(joined, spans, offset, offset+len(canonical))
		if masked != nil {
			if !json.Valid([]byte(*masked)) {
				return nil, false // refuse to emit unmasked structured content
			}
			var rewritten json.RawMessage = json.RawMessage(*masked)
			body["structuredContent"] = rewritten
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(body)
	return out, err == nil
}

// applyMaskToRawJSON rewrites a raw JSON document (tools/call params) with the
// composed spans; the masked document must remain valid JSON.
func applyMaskToRawJSON(raw *json.RawMessage, result *guardrail.CheckResponse) bool {
	if len(result.MaskedSpans) == 0 {
		return false
	}
	masked := maskTextRange(string(*raw), result.MaskedSpans, 0, len(*raw))
	if masked == nil {
		return true // nothing touched — fine
	}
	if !json.Valid([]byte(*masked)) {
		return false
	}
	*raw = json.RawMessage(*masked)
	return true
}

func jsonToString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return "", false
}

// maskTextRange applies non-overlapping spans to the [start,end) fragment of
// content and returns the masked fragment, or nil when no span touches it.
func maskTextRange(content string, spans []guardrail.Span, start, end int) *string {
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
		e := sp.End
		if e > end {
			e = end
		}
		if s > last {
			b.WriteString(content[last:s])
		}
		b.WriteString(sp.Replacement)
		last = e
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

// mcpResultHasUninspectableContent reports whether a tools/call result body
// carries non-text content parts (resource, image, audio, embedded files...)
// or non-text structuredContent leaves that the text guardrail cannot check.
func mcpResultHasUninspectableContent(raw []byte) bool {
	var body struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	for _, p := range body.Content {
		if p.Type != "" && p.Type != "text" {
			return true
		}
	}
	return false
}
