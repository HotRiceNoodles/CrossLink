package mcp

import (
	"crypto/tls"
	"net/http"
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

// newSharedMCPTransport builds the *http.Transport shared by MCP HTTP/SSE
// transports, with SSRF-safe dialers wired when the guard is enabled.
func newSharedMCPTransport(maxIdleConns int) *http.Transport {
	if maxIdleConns <= 0 {
		maxIdleConns = 10
	}
	t := &http.Transport{
		MaxIdleConnsPerHost: maxIdleConns,
		MaxIdleConns:        maxIdleConns,
		IdleConnTimeout:     90 * time.Second,
	}
	if outboundSSRFGuard {
		t.DialContext = guardrail.NewSSRFSafeDialer(mcpSSRFDialTimeout)
		t.DialTLSContext = guardrail.NewSSRFSafeTLSDialer(mcpSSRFDialTimeout, nil)
	}
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
