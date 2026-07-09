package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/guardrail"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	defaultMaxConnsPerHost       = 200
	defaultMaxIdleConnsPerHost   = 20
	defaultMaxIdleConns          = 400
	defaultIdleConnTimeout       = 90 * time.Second
	defaultResponseHeaderTimeout = 60 * time.Second
	defaultSSRFDialTimeout       = 30 * time.Second
)

// outboundSSRFGuard gates whether provider outbound transports apply SSRF
// filtering (loopback/private/link-local blocked, DNS-rebinding-safe). It is
// secure-by-default (true) in production builds. Tests flip it false via TestMain
// so they can target httptest servers on the loopback interface; _test.go files
// are excluded from production binaries, so this is never disabled in deployment.
var outboundSSRFGuard = true

func newStreamTransport() *http.Transport {
	t := &http.Transport{
		MaxConnsPerHost:       defaultMaxConnsPerHost,
		MaxIdleConns:          defaultMaxIdleConns,
		MaxIdleConnsPerHost:   defaultMaxIdleConnsPerHost,
		IdleConnTimeout:       defaultIdleConnTimeout,
		ResponseHeaderTimeout: defaultResponseHeaderTimeout,
	}
	if outboundSSRFGuard {
		t.DialContext = guardrail.NewSSRFSafeDialer(defaultSSRFDialTimeout)
		t.DialTLSContext = guardrail.NewSSRFSafeTLSDialer(defaultSSRFDialTimeout, &tls.Config{NextProtos: []string{"http/1.1"}})
	} else {
		t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &tls.Dialer{Config: &tls.Config{NextProtos: []string{"http/1.1"}}}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return t
}

func newDefaultTransport() *http.Transport {
	t := &http.Transport{
		MaxConnsPerHost:     defaultMaxConnsPerHost,
		MaxIdleConns:        defaultMaxIdleConns,
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost,
		IdleConnTimeout:     defaultIdleConnTimeout,
	}
	if outboundSSRFGuard {
		t.DialContext = guardrail.NewSSRFSafeDialer(defaultSSRFDialTimeout)
		t.DialTLSContext = guardrail.NewSSRFSafeTLSDialer(defaultSSRFDialTimeout, nil)
	}
	return t
}

// ── Transport factory functions ──

// newWrappedTransport wraps base with otelhttp then debugTransport.
func newWrappedTransport(base http.RoundTripper) http.RoundTripper {
	wrapped := otelhttp.NewTransport(base)
	return &debugTransport{base: wrapped}
}

func newDefaultClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: guardrail.BlockInternalRedirect,
		Transport:     newWrappedTransport(newDefaultTransport()),
	}
}

func newStreamClient() *http.Client {
	return &http.Client{
		CheckRedirect: guardrail.BlockInternalRedirect,
		Transport:     newWrappedTransport(newStreamTransport()),
	}
}

// ── debugTransport ──

// debugTransport intercepts upstream HTTP calls when debug mode is enabled.
// It reads the UpstreamCollector from the request context; if nil, it passes
// through with zero overhead.
type debugTransport struct {
	base http.RoundTripper
}

func (t *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	collector := upstream.UpstreamCollectorFromContext(req.Context())
	if collector == nil {
		return t.base.RoundTrip(req)
	}

	start := time.Now()
	call := upstream.UpstreamCall{
		Method:     req.Method,
		Path:       req.URL.Path,
		BaseURL:    req.URL.Scheme + "://" + req.URL.Host,
		ReqHeaders: upstream.RedactHeaders(req.Header),
		Provider:   upstream.ProviderNameFromContext(req.Context()),
		Model:      upstream.ProviderModelFromContext(req.Context()),
		Attempt:    AttemptFromContext(req.Context()),
		IsFallback: upstream.IsFallbackFromContext(req.Context()),
	}
	call.IsRetry = call.Attempt > 1

	// Capture request body: full-read-then-truncate
	if req.Body != nil {
		reqBody, _ := io.ReadAll(io.LimitReader(req.Body, 10*1024*1024))
		if len(reqBody) > upstream.UpstreamBodyLimit {
			call.ReqBody = reqBody[:upstream.UpstreamBodyLimit]
		} else {
			call.ReqBody = reqBody
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	resp, err := t.base.RoundTrip(req)
	call.Duration = time.Since(start)

	if err != nil {
		call.Error = err.Error()
		collector.Add(call)
		return nil, err
	}

	call.StatusCode = resp.StatusCode
	call.RespHeaders = upstream.RedactHeaders(resp.Header)

	// Capture response body
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Streaming: wrap body with captureReadCloser
		wrapper := &captureReadCloser{
			source: resp.Body,
			limit:  upstream.StreamBodyLimit,
		}
		wrapper.onClose = func() {
			call.RespBody = bytes.Clone(wrapper.buf.Bytes())
			collector.Add(call)
		}
		resp.Body = wrapper
	} else {
		// Non-streaming: full-read-then-truncate, restore body
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		if len(body) > upstream.UpstreamBodyLimit {
			call.RespBody = body[:upstream.UpstreamBodyLimit]
		} else {
			call.RespBody = body
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		collector.Add(call)
	}

	return resp, nil
}

// ── captureReadCloser ──

// captureReadCloser wraps an io.ReadCloser to capture the first `limit` bytes
// during reads. The onClose callback is invoked when Close() is called.
// Thread-safe for concurrent Read/Close (e.g. OpenAI's context-cancel goroutine).
type captureReadCloser struct {
	source   io.ReadCloser
	buf      bytes.Buffer
	limit    int
	captured int
	mu       sync.Mutex
	closed   bool
	onClose  func()
}

func (c *captureReadCloser) Read(p []byte) (int, error) {
	n, err := c.source.Read(p) // outside mutex: network I/O, may block
	if n > 0 {
		c.mu.Lock()
		if c.captured < c.limit {
			toCapture := n
			if remaining := c.limit - c.captured; toCapture > remaining {
				toCapture = remaining
			}
			c.buf.Write(p[:toCapture])
			c.captured += toCapture
		}
		c.mu.Unlock()
	}
	return n, err // NEVER modify n or err
}

func (c *captureReadCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.source.Close()
	if c.onClose != nil {
		c.onClose()
	}
	return err
}
