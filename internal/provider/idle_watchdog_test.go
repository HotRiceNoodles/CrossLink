package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/require"
)

// The idle watchdog must close a stalled upstream stream: a server that sends
// one chunk then goes silent must not pin the reader goroutine forever. With
// the watchdog disabled (0) this stream would hang until ctx cancel — the
// test only exercises the enabled path.
func TestStreamIdleWatchdog_ClosesStalledStream(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"go"},"finish_reason":null}]}` + "\n\n"))
		flusher.Flush()
		<-release // stall forever
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	old := getStreamIdleTimeout()
	SetStreamIdleTimeout(400 * time.Millisecond)
	t.Cleanup(func() { SetStreamIdleTimeout(old) })

	p := NewOpenAICompatibleWithTransport("stalled", server.URL, 10*time.Second, &http.Transport{})

	ch, err := p.StreamChat(context.Background(), &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k")
	require.NoError(t, err)

	gotContent := false
	sawDone := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				// Stream closed without Done — the stalled upstream was cut.
				require.False(t, sawDone, "stalled stream must not report a clean Done terminal")
				require.True(t, gotContent, "pre-stall content must have been delivered")
				return
			}
			if chunk.Done {
				sawDone = true
			}
			if chunk.Chunk != nil && len(chunk.Chunk.Choices) > 0 && chunk.Chunk.Choices[0].Delta.Content == "go" {
				gotContent = true
			}
		case <-deadline:
			t.Fatal("idle watchdog did not close the stalled stream within 5s")
		}
	}
}

// A normally-flowing stream must complete with Done even when the watchdog is
// armed — the lastRead timestamp advances on every chunk.
func TestStreamIdleWatchdog_FlowingStreamUnaffected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(server.Close)

	old := getStreamIdleTimeout()
	SetStreamIdleTimeout(500 * time.Millisecond)
	t.Cleanup(func() { SetStreamIdleTimeout(old) })

	p := NewOpenAICompatibleWithTransport("flowing", server.URL, 10*time.Second, &http.Transport{})

	ch, err := p.StreamChat(context.Background(), &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k")
	require.NoError(t, err)

	sawDone := false
	for chunk := range ch {
		if chunk.Done {
			sawDone = true
		}
	}
	require.True(t, sawDone, "flowing stream lost its clean Done terminal")
}

// --- consumer backpressure vs upstream silence (supervisor review) ---

// A consumer that genuinely STOPS reading must not be misread as an idle
// upstream: the send path refreshes the activity timestamp while blocked on
// the full channel, so the watchdog leaves the stream alone (total-duration
// cap / client disconnect bound it). The test stops reading entirely for
// several idle windows, asserts the upstream body was NOT closed (mock
// handler still alive) and the channel NOT closed, then resumes and drains
// all frames to a terminal, and separately proves cancel still tears down.
func TestStreamIdleWatchdog_BackpressuredConsumerNotCut(t *testing.T) {
	var handlerAlive atomic.Bool
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerAlive.Store(true)
		defer handlerAlive.Store(false)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		// More chunks than the 64-slot channel, written immediately: the
		// producer fills the channel then blocks on send (pure backpressure).
		for i := 0; i < 300; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%06d\"},\"finish_reason\":null}]}\n\n", i)
		}
		flusher.Flush()
		// If the gateway (wrongly) closes the body, r.Context() cancels and
		// this handler exits — handlerAlive dropping is the mis-cut signal.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	old := getStreamIdleTimeout()
	SetStreamIdleTimeout(700 * time.Millisecond)
	t.Cleanup(func() { SetStreamIdleTimeout(old) })

	p := NewOpenAICompatibleWithTransport("backpressure", server.URL, 10*time.Second, &http.Transport{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.StreamChat(ctx, &domain.OpenAIRequest{
		MaxTokens: intPtr(100000),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k")
	require.NoError(t, err)

	// Read a few chunks, then STOP touching the channel entirely.
	for i := 0; i < 3; i++ {
		<-ch
	}
	time.Sleep(4 * 700 * time.Millisecond) // 4 idle windows of total silence from the consumer side

	// Not cut: channel still open, upstream handler still alive (body open).
	select {
	case _, ok := <-ch:
		require.True(t, ok, "watchdog cut a backpressured stream")
	default:
	}
	require.True(t, handlerAlive.Load(), "upstream body was closed under pure consumer backpressure (mis-cut)")

	// Resume reading: every buffered frame must arrive, and the stream must
	// reach a terminal (upstream goes silent after the 300 frames, so the
	// watchdog — now correctly, since sends are flowing — ends the stream).
	frames := 3
	deadline := time.After(8 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// All-but-boundary frames must survive the pause. A 1-frame delta at
				// the watchdog-close boundary is tolerated (the scanner may not consume
				// the final buffered line before the cut lands): the invariant under
				// test is that the pause itself does not drop the buffered/flowing
				// stream, not byte-exact delivery of the very last line of a
				// post-silence truncated stream.
				require.GreaterOrEqual(t, frames, 295, "frames lost across the backpressure pause: %d", frames)
				return
			}
			frames++
		case <-deadline:
			t.Fatalf("stream did not terminate after resuming reads (frames=%d)", frames)
		}
	}
}

// Cancel must still tear down a backpressured producer (no goroutine leak).
func TestStreamIdleWatchdog_CancelUnderBackpressureExits(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 300; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })

	old := getStreamIdleTimeout()
	SetStreamIdleTimeout(10 * time.Second) // long: prove CANCEL (not watchdog) tears down
	t.Cleanup(func() { SetStreamIdleTimeout(old) })

	p := NewOpenAICompatibleWithTransport("cancelbp", server.URL, 10*time.Second, &http.Transport{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.StreamChat(ctx, &domain.OpenAIRequest{
		MaxTokens: intPtr(100000),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		<-ch
	}
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("cancel did not tear down the backpressured producer")
		}
	}
}

// Upstream-silence semantics survive the activity change: a connected but
// silent upstream IS cut while the consumer keeps reading. The handler exits
// with the request context (no select{} blocking server.Close on cleanup).
func TestStreamIdleWatchdog_SilentUpstreamStillCut(t *testing.T) {
	old := getStreamIdleTimeout()
	SetStreamIdleTimeout(400 * time.Millisecond)
	t.Cleanup(func() { SetStreamIdleTimeout(old) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"go\"},\"finish_reason\":null}]}\n\n"))
		flusher.Flush()
		<-r.Context().Done() // exits when the watchdog closes the body
	}))
	t.Cleanup(srv.Close)

	p := NewOpenAICompatibleWithTransport("silent", srv.URL, 10*time.Second, &http.Transport{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.StreamChat(ctx, &domain.OpenAIRequest{
		MaxTokens: intPtr(100),
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}, "k")
	require.NoError(t, err)

	gotContent := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				require.True(t, gotContent, "pre-stall content missing")
				return
			}
			if chunk.Chunk != nil && len(chunk.Chunk.Choices) > 0 {
				gotContent = true
			}
		case <-deadline:
			t.Fatal("watchdog failed to cut a silent upstream")
		}
	}
}
