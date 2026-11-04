package middleware

import (
	"context"
	"strings"

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
)

// installIncrementalStream is shared by gateway and admin/playground routes.
// Only SSE bodies are intercepted; JSON errors and non-stream responses retain
// their normal encoding. The original Gin writer remains owned by this request.
func installIncrementalStream(c *gin.Context, svc *guardrail.GuardrailService, meta guardrail.CheckRequest) (func(), error) {
	stream, err := svc.NewResponseStream(c.Request.Context(), meta)
	if err != nil {
		return nil, err
	}
	if stream.Fallback {
		stream.Close()
		c.Header("X-Guardrail-Stream-Mode", "strict")
		c.Header("X-Guardrail-Stream-Fallback", "unsupported_engine")
		return func() {}, nil
	}
	ctx, cancel := context.WithCancel(context.WithValue(c.Request.Context(), guardrail.CtxKeyIncremental, true))
	c.Request = c.Request.WithContext(ctx)
	c.Header("X-Guardrail-Stream-Mode", "incremental")
	c.Header("X-Guardrail-Stream-Hold", strings.Join(stream.HoldReasons(), ","))
	original := c.Writer
	w := &incrementalResponseWriter{ResponseWriter: original, c: c, cancel: cancel}
	w.gate = guardrail.NewIncrementalSSEWriter(c.Request.Context(), stream, original, original.Flush)
	c.Writer = w
	return func() {
		defer cancel()
		if w.active {
			_ = w.gate.Close()
			w.observe()
		} else {
			stream.Close()
		}
		c.Writer = original
	}, nil
}

type incrementalResponseWriter struct {
	gin.ResponseWriter
	c      *gin.Context
	gate   *guardrail.IncrementalSSEWriter
	active bool
	cancel context.CancelFunc
}

func (w *incrementalResponseWriter) Write(p []byte) (int, error) {
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		return w.ResponseWriter.Write(p)
	}
	w.active = true
	n, err := w.gate.Write(p)
	w.observe()
	if err != nil && w.cancel != nil {
		w.cancel()
	}
	return n, err
}
func (w *incrementalResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *incrementalResponseWriter) Flush()                            { w.ResponseWriter.Flush() }
func (w *incrementalResponseWriter) observe() {
	if result := w.gate.Result; result != nil {
		w.c.Set("guardrail_triggered", true)
		w.c.Set("guardrail_rule", result.RuleName)
		setSecurityEvent(w.c, result, "")
	}
}
