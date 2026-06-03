package upstream

import (
	"context"
	"net/http"
	"sync"
)

// UpstreamCollector is a thread-safe collector for upstream HTTP call records.
type UpstreamCollector struct {
	mu         sync.Mutex
	calls      []UpstreamCall
	totalBytes int
}

// Add adds an upstream call record.
// When UpstreamTotalLimit is exceeded, bodies are cleared but metadata is preserved.
func (c *UpstreamCollector) Add(call UpstreamCall) {
	c.mu.Lock()
	defer c.mu.Unlock()

	callBytes := len(call.ReqBody) + len(call.RespBody)
	if c.totalBytes+callBytes > UpstreamTotalLimit {
		call.ReqBody = nil
		call.RespBody = nil
	} else {
		c.totalBytes += callBytes
	}
	c.calls = append(c.calls, call)
}

// Calls returns a snapshot of all collected upstream calls.
func (c *UpstreamCollector) Calls() []UpstreamCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]UpstreamCall, len(c.calls))
	copy(result, c.calls)
	return result
}

// ── Collector context injection ──

type upstreamCollectorKey struct{}

func WithUpstreamCollector(ctx context.Context, c *UpstreamCollector) context.Context {
	return context.WithValue(ctx, upstreamCollectorKey{}, c)
}

func UpstreamCollectorFromContext(ctx context.Context) *UpstreamCollector {
	c, _ := ctx.Value(upstreamCollectorKey{}).(*UpstreamCollector)
	return c
}

// ── Sensitive header redaction ──

var sensitiveHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"Cookie",
	"Set-Cookie",
	"Proxy-Authorization",
}

// RedactHeaders returns a copy of h with sensitive headers removed.
func RedactHeaders(h http.Header) http.Header {
	clone := h.Clone()
	for _, key := range sensitiveHeaders {
		clone.Del(key)
	}
	return clone
}
