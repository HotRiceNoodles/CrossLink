package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// AssemblerHook is an optional callback invoked by ContextAssembler after resolving
// the template + variables but before rendering. Community ships it as nil.
// Enterprise overlay sets it to enforce per-Key / per-Team template permissions
// (e.g. "this key may only use templates in set X"). Returning an error aborts
// the request with 403 before any assembly or upstream call.
type AssemblerHook func(c *gin.Context, tpl *model.PromptTemplate, variables map[string]any) error

// ContextAssembler expands the `x_context` field of a gateway request into a
// full system prompt + few-shot, then removes the field before the request
// continues. Requests without `x_context` pass through unchanged (zero
// regression). Must run AFTER ReadBody (reads BodyKey) and BEFORE Cache/Guardrails
// so downstream sees the assembled request. See
// docs/plans/2026-07-14-context-engineering-gateway-design.md.
// assemblerMaxBody caps the EXPANDED request body (matches the ReadBody
// per-request ceiling): the pre-marshal estimate and the post-marshal
// SetBodyBytes check both enforce it, so a template expansion can never
// allocate past the body envelope before being rejected.
const assemblerMaxBody = 10 << 20

func ContextAssembler(reg *service.TemplateRegistry, hook AssemblerHook) gin.HandlerFunc {
	return func(c *gin.Context) {
		body := GetBodyBytes(c)
		if len(body) == 0 {
			c.Next()
			return
		}
		// Fast path: the x_context field cannot be present unless its literal
		// key appears in the raw bytes — BUT JSON allows ESCAPED key names
		// (`{"\u0078_context":{...}}` unmarshals to the same key and would
		// silently skip template expansion and the permission hook). Escapes
		// require a backslash in the raw bytes, so any backslash falls back
		// to the full parse. This keeps the fast path for escape-free bodies
		// (the overwhelming majority) while preserving exact pre-optimization
		// semantics for every legal JSON encoding of the field name.
		if !bytes.Contains(body, []byte(`"x_context"`)) && !bytes.ContainsRune(body, '\\') {
			c.Next()
			return
		}
		var raw map[string]any
		if json.Unmarshal(body, &raw) != nil {
			c.Next() // not valid JSON — let downstream handle/report
			return
		}
		xc, has := raw["x_context"]
		if !has {
			c.Next() // no template — passthrough
			return
		}
		xcMap, ok := xc.(map[string]any)
		if !ok {
			c.Next()
			return
		}
		name, _ := xcMap["template"].(string)
		if name == "" {
			c.Next()
			return
		}
		variables, _ := xcMap["variables"].(map[string]any)

		tpl, ok := reg.Get(c.Request.Context(), name)
		if !ok {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error_code": "template_not_found", "error": "prompt template not found: " + name})
			return
		}
		// Commercial seam: enterprise overlay may register a hook to enforce
		// per-Key/Team template permissions. nil in Community (no-op).
		if hook != nil {
			if err := hook(c, tpl, variables); err != nil {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error_code": "template_not_allowed", "error": err.Error()})
				return
			}
		}
		rendered, err := service.RenderTemplate(tpl, variables)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error_code": "template_render_error", "error": err.Error()})
			return
		}

		// Always derive format from the request path — the gateway knows the
		// protocol from the endpoint, not from the template. Ignoring the
		// template's target_format avoids a mismatch footgun (e.g. template says
		// anthropic but request hits /v1/chat/completions → system silently lost).
		format := autoFormat(c.Request.URL.Path)
		switch format {
		case "anthropic":
			if _, hasSys := raw["system"]; hasSys && !isNil(raw["system"]) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error_code": "system_conflict", "error": "request already has a system field; remove it or the x_context"})
				return
			}
			raw["system"] = rendered.SystemPrompt
			raw["messages"] = prependMessages(getMessages(raw), rendered.FewShot)
		default: // openai
			msgs := getMessages(raw)
			if hasSystemMessage(msgs) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error_code": "system_conflict", "error": "request already has a system message; remove it or the x_context"})
				return
			}
			injected := []any{gin.H{"role": "system", "content": rendered.SystemPrompt}}
			for _, fs := range rendered.FewShot {
				injected = append(injected, gin.H{"role": fs.Role, "content": fs.Content})
			}
			raw["messages"] = append(injected, msgs...)
		}

		delete(raw, "x_context")

		// PRE-MARSHAL size cap (acceptance blocker 2): json.Marshal below
		// allocates the FULL expanded body before SetBodyBytes can check the
		// ceiling — a giant template would OOM/allocate first, reject later.
		// Conservative estimate BEFORE building: original body bytes plus
		// every injected string at ×6 (worst-case JSON escaping of control
		// characters). The SetBodyBytes ceiling check after Marshal remains
		// the exact backstop.
		est := int64(len(body))
		for _, s := range []string{rendered.SystemPrompt} {
			est += int64(len(s)) * 6
		}
		for _, fs := range rendered.FewShot {
			est += (int64(len(fs.Role)) + int64(len(fs.Content))) * 6
		}
		if est > assemblerMaxBody {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error_code": "template_expansion_too_large",
				"error":      "assembled request exceeds the body-size ceiling; template too large",
			})
			return
		}
		// GLOBAL budget reservation BEFORE the Marshal allocation (acceptance
		// round 2): a per-request ceiling alone does not stop N concurrent
		// expansions; the estimated growth delta leaves the shared budget
		// first. SetBodyBytes consumes this pre-allocation exactly.
		if !PreallocBodyGrowth(c, est-int64(len(body))) {
			budgetExhaustedAbort(c)
			return
		}

		newBody, err := json.Marshal(raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "reassemble request failed"})
			return
		}
		// B1 double-update via SetBodyBytes: BodyKey (GetBodyBytes consumers)
		// AND c.Request.Body (direct readers) — this runs BEFORE Cache which
		// keys on the body. Budget-aware (supervisor F26 review): template
		// expansion may not push the body past the per-request ceiling or the
		// in-flight budget; exceeding either aborts with a typed error.
		if !SetBodyBytes(c, newBody) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error_code": "template_expansion_too_large",
				"error":      "assembled request exceeds the body-size ceiling; template too large",
			})
			return
		}
		c.Request.ContentLength = int64(len(newBody))
		c.Set("template_id", tpl.ID)
		c.Next()
	}
}

func autoFormat(path string) string {
	if path == "/v1/messages" {
		return "anthropic"
	}
	return "openai"
}

func getMessages(raw map[string]any) []any {
	m, ok := raw["messages"].([]any)
	if !ok {
		return nil
	}
	return m
}

func hasSystemMessage(msgs []any) bool {
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if r, _ := mm["role"].(string); r == "system" {
				return true
			}
		}
	}
	return false
}

func prependMessages(msgs []any, fewShot []service.FewShotMessage) []any {
	out := make([]any, 0, len(fewShot)+len(msgs))
	for _, fs := range fewShot {
		out = append(out, gin.H{"role": fs.Role, "content": fs.Content})
	}
	return append(out, msgs...)
}

func isNil(v any) bool {
	return v == nil
}
