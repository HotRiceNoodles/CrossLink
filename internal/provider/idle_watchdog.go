package provider

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// streamIdleTimeout bounds how long an established SSE stream may stay silent
// (no bytes from upstream) before the gateway closes the body and treats the
// stream as interrupted. Without it a connected-but-stalled upstream pins one
// goroutine + one upstream connection per request indefinitely — the only
// other exit is a client disconnect. 0 disables the watchdog (boot default
// comes from server.stream_idle_timeout).
var streamIdleTimeout atomic.Int64 // nanoseconds; 0 = disabled

// streamWatchdogCutsTotal counts watchdog terminations (genuine upstream
// stalls). The E2E backpressure acceptance contrasts this counter with
// cl_stream_backpressure_events_total: backpressure events must NOT move the
// cut counter.
var streamWatchdogCutsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cl_stream_idle_watchdog_cuts_total",
	Help: "SSE streams closed by the idle watchdog (upstream genuinely silent). Backpressured-but-alive streams must never appear here.",
})

// streamBackpressureEventsTotal counts send attempts that stayed blocked on a
// full downstream channel longer than the idle window while the watchdog
// correctly held (positive evidence the stream survived backpressure, not a
// mis-cut). Rate of false cuts is measured against this.
var streamBackpressureEventsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cl_stream_backpressure_events_total",
	Help: "Channel sends blocked by a slow consumer for longer than the idle window; watchdog held and the stream stayed alive.",
})

// lastBackpressureLogNs rate-limits the per-event Info log globally (one line
// per 5s) so a 20MB backpressured stream cannot flood the log with thousands
// of entries while still leaving positive evidence in it.
var lastBackpressureLogNs atomic.Int64

// SetStreamIdleTimeout configures the per-stream idle cap (<= 0 disables it).
func SetStreamIdleTimeout(d time.Duration) { streamIdleTimeout.Store(int64(d)) }

func getStreamIdleTimeout() time.Duration {
	return time.Duration(streamIdleTimeout.Load())
}

// idleReadCloser records the time of the last stream PROGRESS so the
// watchdog can distinguish "upstream silent" from "consumer-backed-up":
// progress = bytes read from upstream OR a send attempt while blocked on a
// full channel (a slow CLIENT is not upstream silence — such streams are
// bounded by the total-duration cap and client disconnects instead).
type idleReadCloser struct {
	rc       io.ReadCloser
	lastRead atomic.Int64 // unix nanos; read via Activity()
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.lastRead.Store(time.Now().UnixNano())
	}
	return n, err
}

func (r *idleReadCloser) Close() error { return r.rc.Close() }

// Activity exposes the shared progress timestamp so the producer loop can
// refresh it while blocked on channel sends (write backpressure).
func (r *idleReadCloser) Activity() *atomic.Int64 { return &r.lastRead }

// watchStreamIdle wraps rc and starts a watchdog goroutine that closes the
// underlying body once no bytes have arrived for the configured idle
// duration, unblocking a scanner parked in Read. The returned stop function
// must be called when reading ends (defer in the producer goroutine) so the
// watchdog exits IMMEDIATELY (a ticker-based design would linger one interval
// per stream and show up as transient goroutine growth). The watchdog also
// exits on stall close, making stop idempotent.
func watchStreamIdle(rc io.ReadCloser, idle time.Duration) (*idleReadCloser, func()) {
	w := &idleReadCloser{rc: rc}
	w.lastRead.Store(time.Now().UnixNano())
	stopped := make(chan struct{})
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-timer.C:
				// No progress (no upstream bytes AND no blocked-send retries)
				// since the timer was armed → genuinely stalled upstream.
				// Slow-but-flowing consumers keep Activity fresh via the
				// send-path heartbeat and must NOT be cut here.
				if time.Since(time.Unix(0, w.lastRead.Load())) > idle {
					streamWatchdogCutsTotal.Inc()
					slog.Warn("SSE stream stalled: no upstream data within idle timeout, closing body",
						"idle_timeout", idle.String())
					rc.Close() // unblocks the reader; producer's defers stop the watchdog
					return
				}
				timer.Reset(idle)
			}
		}
	}()
	var once sync.Once
	return w, func() { once.Do(func() { close(stopped) }) }
}
