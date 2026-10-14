package guardrail

import (
	"net"
	"sync"
)

// Connection revocation support (B5).
//
// Closing only IDLE pooled connections is not enough: a connection that is
// BUSY at the moment the allowlist narrows returns to the idle pool when its
// request finishes and would then be REUSED by new requests — bypassing the
// new policy without ever dialing — until it ages out. To make "pool reuse
// must not bypass revocation" true, every connection opened through the
// SSRF-safe dialers is tracked with its final socket IP, and on every policy
// swap the connections whose IP the NEW policy blocks are closed outright.
//
// Consequence (explicit design choice, covered by tests): an in-flight
// request to a target that just became blocked is TERMINATED at revocation
// time; in-flight requests to still-allowed targets are never touched.

var (
	trackedMu  sync.Mutex
	tracked    = map[*trackedConn]net.IP{}
	nextConnID int64
)

// trackedConn removes itself from the registry on close so the map only ever
// holds live sockets.
type trackedConn struct {
	net.Conn
	id int64
}

func trackConn(c net.Conn, ip net.IP) net.Conn {
	if ip == nil {
		return c
	}
	tc := &trackedConn{Conn: c}
	trackedMu.Lock()
	nextConnID++
	tc.id = nextConnID
	tracked[tc] = ip
	trackedMu.Unlock()
	return tc
}

func (c *trackedConn) Close() error {
	trackedMu.Lock()
	delete(tracked, c)
	trackedMu.Unlock()
	return c.Conn.Close()
}

// closeRevokedConns closes every tracked connection whose remote IP the
// CURRENT policy blocks. Called after each allowlist swap.
func closeRevokedConns() {
	trackedMu.Lock()
	doomed := make([]*trackedConn, 0, len(tracked))
	for tc, ip := range tracked {
		if blockedIP(ip) {
			doomed = append(doomed, tc)
			delete(tracked, tc) // remove first: Close re-enters the lock
		}
	}
	trackedMu.Unlock()
	for _, tc := range doomed {
		_ = tc.Conn.Close()
	}
}

// TrackedConnCount reports the number of live tracked outbound connections
// (diagnostics/tests).
func TrackedConnCount() int {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	return len(tracked)
}
