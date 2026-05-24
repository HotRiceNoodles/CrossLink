package debug

import (
	"bytes"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

type responseCaptureWriter struct {
	gin.ResponseWriter
	body      *bytes.Buffer
	limit     int
	truncated bool
	mu        sync.Mutex
}

func newResponseCaptureWriter(w gin.ResponseWriter, limit int) *responseCaptureWriter {
	return &responseCaptureWriter{
		ResponseWriter: w,
		body:           bytes.NewBuffer(nil),
		limit:          limit,
	}
}

func (w *responseCaptureWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.capture(b)
	return n, err
}

func (w *responseCaptureWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.capture([]byte(s))
	return n, err
}

func (w *responseCaptureWriter) capture(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.body.Len() >= w.limit {
		w.truncated = true
		return
	}
	remaining := w.limit - w.body.Len()
	if len(b) > remaining {
		w.body.Write(b[:remaining])
		w.truncated = true
	} else {
		w.body.Write(b)
	}
}

func (w *responseCaptureWriter) CapturedBody() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Bytes()
}

func (w *responseCaptureWriter) IsTruncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// Flush delegates to the underlying ResponseWriter's Flush method.
func (w *responseCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// CloseNotify delegates to the underlying ResponseWriter's CloseNotify method.
func (w *responseCaptureWriter) CloseNotify() <-chan bool {
	if cn, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return cn.CloseNotify()
	}
	return nil
}
