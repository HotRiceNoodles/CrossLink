package provider

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B5: outbound transports must register themselves so a policy swap drops
// their idle pooled connections, and they must never negotiate HTTP/2 (an
// h2-multiplexed connection could carry NEW requests over a TCP dialed
// under an old, wider policy — revocation would be bypassed until idle).

type countingListener struct {
	net.Listener
	open int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	atomic.AddInt64(&l.open, 1)
	return &countingConn{Conn: c, parent: l}, nil
}

type countingConn struct {
	net.Conn
	parent *countingListener
	closed bool
}

func (c *countingConn) Close() error {
	if !c.closed {
		c.closed = true
		atomic.AddInt64(&c.parent.open, -1)
	}
	return c.Conn.Close()
}

func newCountingHTTPTestServer(t *testing.T) (*httptest.Server, *countingListener) {
	t.Helper()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cl := &countingListener{Listener: base}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = cl
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, cl
}

func waitOpenConns(t *testing.T, cl *countingListener, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&cl.open) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("open server conns = %d, want %d", atomic.LoadInt64(&cl.open), want)
}

func TestOutboundTransports_IdleClosedOnPolicyPublish(t *testing.T) {
	srv, cl := newCountingHTTPTestServer(t)

	tr := newDefaultTransport()
	outboundTransportsMu.Lock()
	registered := false
	for _, x := range outboundTransports {
		if x == tr {
			registered = true
		}
	}
	outboundTransportsMu.Unlock()
	require.True(t, registered, "newDefaultTransport must register in the outbound registry")

	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	waitOpenConns(t, cl, 1) // one idle pooled connection server-side

	// ANY policy publish must close the idle pooled connection via the
	// registered observer (this swap to strict is the narrowing case).
	guardrail.ApplyInternalAllowlist(nil)
	waitOpenConns(t, cl, 0)
}

func TestOutboundTransports_NoHTTP2Upgrade(t *testing.T) {
	// Structural pin: with custom dialers and ForceAttemptHTTP2 unset, Go's
	// http.Transport never offers h2 → no multiplexed reuse across a policy
	// change. If someone enables h2 here, this test forces the revocation
	// semantics discussion the code comments warn about.
	for _, tr := range []*http.Transport{newDefaultTransport(), newStreamTransport()} {
		assert.False(t, tr.ForceAttemptHTTP2, "outbound transports must not enable HTTP/2 without re-evaluating allowlist revocation")
	}
}

func TestOutboundTransports_BothBuildersRegister(t *testing.T) {
	before := func() int {
		outboundTransportsMu.Lock()
		defer outboundTransportsMu.Unlock()
		return len(outboundTransports)
	}()
	_ = newStreamTransport()
	_ = newDefaultTransport()
	outboundTransportsMu.Lock()
	n := len(outboundTransports)
	outboundTransportsMu.Unlock()
	assert.GreaterOrEqual(t, n, before+2, "both transport builders register in the shared registry")
}
