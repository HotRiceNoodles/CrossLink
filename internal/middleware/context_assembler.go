package middleware

import (
	"bytes"
	"encoding/json"
	"io"
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
func ContextAssembler(reg *service.TemplateRegistry, hook AssemblerHook) gin.HandlerFunc {
	return func(c *gin.Context) {
		body := GetBodyBytes(c)
		if len(body) == 0 {
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

		format := tpl.TargetFormat
		if format == "" || format == "auto" {
			format = autoFormat(c.Request.URL.Path)
		}
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
		newBody, err := json.Marshal(raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "reassemble request failed"})
			return
		}
		// B1 double-update: both BodyKey (for GetBodyBytes) and c.Request.Body
		// (for direct readers) must reflect the assembled body, since this runs
		// BEFORE Cache which keys on the body.
		c.Set(BodyKey, newBody)
		c.Request.Body = io.NopCloser(bytes.NewReader(newBody))
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
