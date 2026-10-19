package app

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
)

// TestInitRedis_DisablesCommandRetries pins the EFFECTIVE retry setting of
// the constructed client. go-redis v9.18.0 Options.init() normalizes
// MaxRetries: -1 -> 0 (disabled); 0 -> 3 (default!). A previous attempt set
// 0 explicitly and silently ran WITH retries (review catch) — this test
// fails on any regression to that shape, including someone "simplifying"
// -1 back to 0.
func TestInitRedis_DisablesCommandRetries(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // never speak RESP; the startup Ping fails harmlessly
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	rdb := InitRedis(&config.RedisConfig{Host: "127.0.0.1", Port: port})
	defer rdb.Close()

	if got := rdb.Options().MaxRetries; got != 0 {
		t.Fatalf("effective MaxRetries = %d, want 0 (disabled). go-redis normalizes 0->3; the option must be set to -1", got)
	}
}

// TestRedisClient_FaultedCommandExecutesOnce proves the wiring end-to-end
// against a "paused server" endpoint (kernel accepts, bytes swallowed, no
// reply): each client op makes EXACTLY ONE connection attempt. go-redis
// v9 handshakes (HELLO) before issuing any command, so the swallowed
// endpoint never sees the command itself — the wire-observable retry
// signature is the number of connection attempts per op. With the default
// 3 retries, a timing-out op redials up to 4 times (each redial's bytes
// execute when a paused Redis resumes — the double-apply mechanism for
// non-idempotent budget writes); with retries disabled (-1) it is 1.
func TestRedisClient_FaultedCommandExecutesOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer ln.Close()

	var conns int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&conns, 1)
			// Swallow bytes so the client's read times out, then close.
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	rdb := InitRedis(&config.RedisConfig{Host: "127.0.0.1", Port: port})
	defer rdb.Close()

	// One marker GET after startup: startup ping (1 attempt, fails, warned)
	// + the GET itself (must be exactly 1 more). A retrying client (the
	// normalized 0->3 default) makes up to 4 attempts per op.
	gctx, gcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer gcancel()
	if err := rdb.Get(gctx, "cl-retry-once-marker").Err(); err == nil {
		t.Fatal("expected an error against the swallowing endpoint")
	}
	time.Sleep(200 * time.Millisecond) // let any (forbidden) redials land

	got := atomic.LoadInt32(&conns)
	if got > 2 {
		t.Fatalf("connection attempts = %d for (startup ping + one GET), want <= 2 — retries are NOT disabled (go-redis normalizes MaxRetries 0->3; the option must be -1)", got)
	}
	if got < 2 {
		t.Fatalf("connection attempts = %d, want exactly 2 (startup ping + GET); test endpoint misbehaved", got)
	}
}
