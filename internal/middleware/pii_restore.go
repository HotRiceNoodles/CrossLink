package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
)

// The restore writer is installed before the moderation writer: output checks
// and handler usage records see only masked text; restoration is the final hop.
func installPIIRestore(c *gin.Context, vault *guardrail.PIIVault, maxBytes, maxEvents int) func() {
	original := c.Writer
	w := &piiRestoreWriter{ResponseWriter: original, vault: vault, maxBytes: maxBytes}
	w.sse = guardrail.NewPIIRestoreSSE(original, vault, maxBytes, maxEvents)
	c.Writer = w
	return func() {
		defer func() { c.Writer = original }()
		if c.Request.Context().Err() != nil {
			return
		}
		if err := w.close(); err != nil {
			// Never log content, tokens, or the vault.
			slog.Warn("guardrail: PII response restoration failed")
			if !original.Written() {
				original.Header().Del("Content-Length")
				original.Header().Set("Content-Type", "application/json")
				original.WriteHeader(502)
				_, _ = original.WriteString(`{"error":{"type":"pii_restore_error","message":"response restoration failed"}}`)
			} else if w.stream {
				_, _ = original.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"pii_restore_error\",\"message\":\"response restoration failed\"}}\n\n")
				original.Flush()
			}
		}
	}
}

type piiRestoreWriter struct {
	gin.ResponseWriter
	vault          *guardrail.PIIVault
	sse            *guardrail.PIIRestoreSSE
	body           bytes.Buffer
	maxBytes       int
	stream, failed bool
}

func (w *piiRestoreWriter) WriteHeader(code int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(code)
}
func (w *piiRestoreWriter) WriteHeaderNow() {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeaderNow()
}
func (w *piiRestoreWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *piiRestoreWriter) Flush() {
	w.Header().Del("Content-Length")
	w.ResponseWriter.Flush()
}
func (w *piiRestoreWriter) Write(p []byte) (int, error) {
	w.Header().Del("Content-Length")
	if w.failed {
		return 0, io.ErrClosedPipe
	}
	if w.Status() >= 400 {
		return w.ResponseWriter.Write(p)
	}
	typ := w.Header().Get("Content-Type")
	if strings.HasPrefix(typ, "text/event-stream") {
		w.stream = true
		n, err := w.sse.Write(p)
		if err != nil {
			w.failed = true
		}
		return n, err
	}
	if !strings.Contains(typ, "json") {
		return w.ResponseWriter.Write(p)
	}
	if w.body.Len()+len(p) > w.maxBytes {
		w.failed = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(p)
}
func (w *piiRestoreWriter) close() error {
	if w.failed {
		return io.ErrClosedPipe
	}
	if w.stream {
		err := w.sse.Close()
		w.ResponseWriter.Flush()
		return err
	}
	if w.body.Len() == 0 {
		return nil
	}
	v, err := guardrail.DecodePIIJSON(w.body.Bytes())
	if err != nil {
		return err
	}
	guardrail.RestorePIIJSON(v, w.vault)
	p, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Del("Content-Length")
	_, err = w.ResponseWriter.Write(p)
	return err
}
