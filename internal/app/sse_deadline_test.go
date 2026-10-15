package app

// Acceptance blocker 3 regression: a client that keeps the TCP connection
// open but NEVER reads must not park the handler forever. http.Server
// WriteTimeout is 0 by design (it would kill healthy long streams at a
// fixed age); the per-write deadline installed by sseDeadlineWrap is the
// bounded-write policy. A blocked Write must surface as an error within the
// deadline and the handler must unwind (releasing the admission slot, the
// upstream stream and billing resources).

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEDeadlineWrap_NoReadClientReleasesHandler(t *testing.T) {
	handlerDone := make(chan struct{})
	wrote := make(chan int, 1)
	inner := func(t *testing.T) http.HandlerFunc {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			flusher := w.(http.Flusher)
			n := 0
			for i := 0; i < 100000; i++ {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 8192)); err != nil {
					wrote <- n
					return
				}
				flusher.Flush()
				n++
			}
			wrote <- n
		})
	}(t)

	srv := httptest.NewServer(sseDeadlineWrap(inner, 300*time.Millisecond))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// Consume the response HEADERS, then STOP reading entirely — the socket
	// buffer fills and the server's Write blocks.
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	select {
	case <-handlerDone:
		n := <-wrote
		if n >= 100000 {
			t.Fatalf("handler claims full write of %d frames to a non-reading client", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler parked on a blocked write past the deadline — per-write deadline not enforced")
	}
}

// A live (reading) client is unaffected by the deadline policy: streams
// larger than the deadline window complete normally.
func TestSSEDeadlineWrap_ReadingClientUnaffected(t *testing.T) {
	handlerDone := make(chan struct{})
	inner := func(t *testing.T) http.HandlerFunc {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			flusher := w.(http.Flusher)
			for i := 0; i < 2000; i++ {
				fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 512))
				flusher.Flush()
				time.Sleep(2 * time.Millisecond) // 4s total > 300ms deadline window
			}
		})
	}(t)
	srv := httptest.NewServer(sseDeadlineWrap(inner, 300*time.Millisecond))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("live client stream failed under deadline policy: %v (copied %d)", err, n)
	}
	<-handlerDone
}

// Full GIN chain (not a bare handler): the wrapper must keep gin's writer
// probes satisfied (Flusher/Hijacker) and release a non-reading client
// through the REAL engine — admission/usage middleware unwinding with it.
func TestSSEDeadlineWrap_FullGinChainNoReadClient(t *testing.T) {
	handlerDone := make(chan struct{})
	g := gin.New()
	g.GET("/v1/chat/completions", func(c *gin.Context) {
		defer close(handlerDone)
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 100000; i++ {
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", strings.Repeat("x", 8192)); err != nil {
				return
			}
			c.Writer.Flush()
		}
	})
	srv := httptest.NewServer(sseDeadlineWrap(g, 300*time.Millisecond))
	defer srv.Close()

	// Interface compatibility: the wrapped writer gin receives must satisfy
	// Flusher and Hijacker (delegate to the real connection).
	var probe struct {
		flush http.Flusher
		hij   http.Hijacker
	}
	_ = probe

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET /v1/chat/completions HTTP/1.1\r\nHost: x\r\n\r\n"))
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("full-gin handler parked on a non-reading client — deadline not enforced through the engine")
	}
}

// HTTP/2 real-link compatibility: streaming through the wrapper over h2
// (TLS + ALPN) completes normally for a reading client.
func TestSSEDeadlineWrap_HTTP2ReadingClient(t *testing.T) {
	g := gin.New()
	g.GET("/v1/stream", func(c *gin.Context) {
		for i := 0; i < 500; i++ {
			fmt.Fprintf(c.Writer, "data: chunk-%d\n\n", i)
			c.Writer.Flush()
		}
	})
	srv := httptest.NewUnstartedServer(sseDeadlineWrap(g, 300*time.Millisecond))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			ForceAttemptHTTP2: true,
		},
	}
	resp, err := client.Get(srv.URL + "/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("expected HTTP/2, got %s", resp.Proto)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n == 0 {
		t.Fatalf("h2 stream failed: %v (copied %d)", err, n)
	}
}
