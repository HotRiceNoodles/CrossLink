package mcp

import (
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/crosslink/internal/guardrail"
)

const mcpSSRFDialTimeout = 30 * time.Second

// outboundSSRFGuard gates whether MCP outbound transports apply SSRF filtering
// (loopback/private/link-local blocked, DNS-rebinding-safe). Secure-by-default
// (true) in production; tests flip it false via TestMain to target httptest
// servers on loopback. _test.go files are excluded from production binaries, so
// this is never disabled in deployment.
var outboundSSRFGuard = true

// MCP outbound transports register here so an allowlist swap drops the idle
// pooled connections the previous, wider policy admitted (revocation must not
// be bypassed by pool reuse). Busy connections are untouched; MCP transports
// never negotiate HTTP/2 (custom dialers, no ForceAttemptHTTP2 — the fallback
// transport additionally pins http/1.1), so no multiplexed stream can carry a
// NEW request over a connection dialed under an old policy.
var (
	outboundTransportsMu sync.Mutex
	outboundTransports   []*http.Transport
	observerOnce         sync.Once
)

func registerOutboundTransport(t *http.Transport) {
	observerOnce.Do(func() {
		guardrail.OnInternalAllowlistApply(func() {
			outboundTransportsMu.Lock()
			ts := make([]*http.Transport, len(outboundTransports))
			copy(ts, outboundTransports)
			outboundTransportsMu.Unlock()
			for _, tr := range ts {
				tr.CloseIdleConnections()
			}
		})
	})
	outboundTransportsMu.Lock()
	outboundTransports = append(outboundTransports, t)
	outboundTransportsMu.Unlock()
}

// CloseIdleOutboundConnections closes all idle pooled MCP connections
// (invoked automatically on allowlist publish; exposed for tests/tooling).
func CloseIdleOutboundConnections() {
	outboundTransportsMu.Lock()
	ts := make([]*http.Transport, len(outboundTransports))
	copy(ts, outboundTransports)
	outboundTransportsMu.Unlock()
	for _, tr := range ts {
		tr.CloseIdleConnections()
	}
}

// newSharedMCPTransport builds the *http.Transport shared by MCP HTTP/SSE
// transports, with SSRF-safe dialers wired when the guard is enabled.
// ResponseHeaderTimeout is the handshake bound — the clients built on this
// transport deliberately carry NO overall http.Client.Timeout because
// streamable-HTTP tool results arrive as SSE bodies that may legitimately run
// minutes; the per-call request context bounds their lifetime.
func newSharedMCPTransport(maxIdleConns int) *http.Transport {
	if maxIdleConns <= 0 {
		maxIdleConns = 10
	}
	t := &http.Transport{
		MaxIdleConnsPerHost:   maxIdleConns,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if outboundSSRFGuard {
		t.DialContext = guardrail.NewSSRFSafeDialer(mcpSSRFDialTimeout)
		t.DialTLSContext = guardrail.NewSSRFSafeTLSDialer(mcpSSRFDialTimeout, nil)
	}
	registerOutboundTransport(t)
	return t
}

// newFallbackMCPTransport is used when no shared transport is supplied (e.g.
// ad-hoc transports in tests). It applies the same SSRF protections when armed.
func newFallbackMCPTransport() *http.Transport {
	t := &http.Transport{}
	if outboundSSRFGuard {
		t.DialContext = guardrail.NewSSRFSafeDialer(mcpSSRFDialTimeout)
		t.DialTLSContext = guardrail.NewSSRFSafeTLSDialer(mcpSSRFDialTimeout, &tls.Config{NextProtos: []string{"http/1.1"}})
	}
	registerOutboundTransport(t)
	return t
}

// mcpHTTPClient wraps a transport in an http.Client that blocks internal
// redirects. Used by all MCP outbound clients.
func mcpHTTPClient(transport *http.Transport, timeout time.Duration) *http.Client {
	c := &http.Client{
		CheckRedirect: guardrail.BlockInternalRedirect,
		Transport:     transport,
	}
	if timeout > 0 {
		c.Timeout = timeout
	}
	return c
}
