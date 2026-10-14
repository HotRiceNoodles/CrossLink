package guardrail

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B5 revocation semantics, proven on a REAL socket against this machine's
// actual private interface address (same technique as the dial e2e test):
//  1. new TCP connections obey the CURRENT policy at dial time;
//  2. idle pooled connections are closed as soon as the policy swaps;
//  3. an in-flight request to a target that REMAINS allowed survives a
//     narrowing swap (narrow 24 → 32);
//  4. an in-flight request to a target that BECOMES blocked is TERMINATED
//     at revocation time — pool reuse can never bypass the new policy.
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

func TestAllowlistRevocation_NewIdleInflight(t *testing.T) {
	restoreStrict(t)
	ip := firstPrivateIPv4(t)
	addr := netip.MustParseAddr(ip.String())
	prefix24 := netip.PrefixFrom(addr, 24).Masked()
	prefix32 := netip.PrefixFrom(addr, 32).Masked()

	holdA := make(chan struct{}) // request that must SURVIVE narrowing
	holdB := make(chan struct{}) // request that must be TERMINATED
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/holda", func(w http.ResponseWriter, _ *http.Request) {
		<-holdA
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/holdb", func(w http.ResponseWriter, _ *http.Request) {
		<-holdB
		w.WriteHeader(http.StatusOK)
	})

	base, err := net.Listen("tcp", ip.String()+":0")
	require.NoError(t, err)
	cl := &countingListener{Listener: base}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(cl) }()
	t.Cleanup(func() {
		select {
		case <-holdA:
		default:
			close(holdA)
		}
		select {
		case <-holdB:
		default:
			close(holdB)
		}
		_ = srv.Close()
	})
	target := fmt.Sprintf("http://%s", cl.Addr().String())

	transport := &http.Transport{DialContext: NewSSRFSafeDialer(5 * time.Second)}
	// Simulate what the provider/MCP packages register on init: drop idle
	// pooled conns whenever the policy swaps.
	OnInternalAllowlistApply(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	waitConns := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if atomic.LoadInt64(&cl.open) == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("open conns = %d, want %d", atomic.LoadInt64(&cl.open), want)
	}

	// 1) Allowed → request succeeds; one pooled (idle) connection remains.
	require.NoError(t, SetInternalAllowlist([]string{prefix24.String()}))
	resp, err := client.Get(target + "/ok")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	waitConns(1)

	// 2) Policy swap to strict closes the IDLE pooled connection at once —
	// not after the 90s idle timeout, and not only on next dial.
	require.NoError(t, SetInternalAllowlist(nil))
	waitConns(0)

	// New requests are refused at the dial layer.
	_, err = client.Get(target + "/ok")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restricted address")

	// 3) In-flight to a STILL-ALLOWED target survives narrowing (/24 → /32).
	require.NoError(t, SetInternalAllowlist([]string{prefix24.String()}))
	doneA := make(chan error, 1)
	go func() {
		resp, err := client.Get(target + "/holda")
		if err == nil {
			resp.Body.Close()
		}
		doneA <- err
	}()
	waitConns(1)
	time.Sleep(150 * time.Millisecond) // ensure the request is in flight
	require.NoError(t, SetInternalAllowlist([]string{prefix32.String()}))
	// New requests still allowed under the narrowed-but-including policy.
	resp, err = client.Get(target + "/ok")
	require.NoError(t, err)
	resp.Body.Close()
	close(holdA)
	select {
	case err := <-doneA:
		require.NoError(t, err, "in-flight request to a still-allowed target must complete")
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request to a still-allowed target did not complete")
	}

	// 4) In-flight to a target that becomes BLOCKED is terminated at
	// revocation. holdb deliberately REUSES one of the idle pooled conns
	// (no new dial) — proving that pool reuse cannot outlive the policy
	// that admitted the connection.
	waitConnsAtLeast(t, cl, 2)
	doneB := make(chan error, 1)
	go func() {
		resp, err := client.Get(target + "/holdb")
		if err == nil {
			resp.Body.Close()
		}
		doneB <- err
	}()
	time.Sleep(250 * time.Millisecond)            // in flight on a reused pooled conn
	require.NoError(t, SetInternalAllowlist(nil)) // target becomes blocked
	select {
	case err := <-doneB:
		require.Error(t, err, "in-flight request to a revoked target must be terminated")
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request to a revoked target was not terminated")
	}
	// The client side was killed above (doneB errored); release the server
	// handler so the server-side socket drains and the count can hit zero.
	close(holdB)
	waitConns(0)
	_, err = client.Get(target + "/ok")
	require.Error(t, err)
}

func waitConnsAtLeast(t *testing.T, cl *countingListener, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&cl.open) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("open conns = %d, want >= %d", atomic.LoadInt64(&cl.open), want)
}
