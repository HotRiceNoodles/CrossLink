package worker

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// tasksDroppedTotal counts tasks dropped because both the pool queue and the
// overflow allowance were exhausted. Dropped usage rows are unrecoverable (no
// persistence fallback), so this counter is the alerting surface for
// saturation: any increase means billing data was lost.
var tasksDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cl_worker_tasks_dropped_total",
	Help: "Tasks dropped because the worker pool queue and overflow allowance were both exhausted, by pool.",
}, []string{"pool"})

// tasksOverflowTotal counts tasks that bypassed the full queue via the
// bounded overflow allowance (detached execution). These still run — the
// counter only shows how often the queue saturated.
var tasksOverflowTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cl_worker_tasks_overflow_total",
	Help: "Tasks run via the overflow allowance because the worker pool queue was full, by pool. These are NOT dropped.",
}, []string{"pool"})

type Pool struct {
	kind    string
	ch      chan func()
	wg      sync.WaitGroup
	dropped atomic.Int64
	closed  atomic.Bool
	// overflow bounds detached execution when the queue is full, so a
	// sustained overload degrades into bounded extra concurrency instead of
	// either dropping tasks (old behavior) or spawning unbounded goroutines.
	overflow chan struct{}
}

func NewPool(kind string, workerCount int, bufferSize int, overflowCap int) *Pool {
	if workerCount <= 0 {
		workerCount = 1
	}
	if bufferSize <= 0 {
		bufferSize = 1
	}
	if overflowCap < 0 {
		overflowCap = 0
	}
	p := &Pool{
		kind:     kind,
		ch:       make(chan func(), bufferSize),
		overflow: make(chan struct{}, overflowCap),
	}
	for i := 0; i < workerCount; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for fn := range p.ch {
				p.run(fn)
			}
		}()
	}
	return p
}

// Dropped reports how many tasks this pool has dropped since startup.
func (p *Pool) Dropped() int64 { return p.dropped.Load() }

func (p *Pool) Submit(fn func()) {
	if p.closed.Load() {
		// Shutdown already ran; sending would panic on the closed channel.
		// Best-effort: run detached so late usage rows still get a chance to
		// flush before the process exits.
		p.runDetached(fn)
		return
	}
	if p.send(fn) {
		return
	}
	// Queue full: fall back to a bounded detached goroutine instead of
	// dropping — a transient DB slowdown should slow the pipeline down, not
	// silently discard billing rows. Only when the overflow allowance is also
	// exhausted is the task dropped (and counted).
	select {
	case p.overflow <- struct{}{}:
		tasksOverflowTotal.WithLabelValues(p.kind).Inc()
		p.runDetachedTracked(fn)
		return
	default:
	}
	p.dropped.Add(1)
	tasksDroppedTotal.WithLabelValues(p.kind).Inc()
	slog.Warn("worker pool full, dropping task", "pool", p.kind, "dropped_total", p.dropped.Load())
}

// runDetachedTracked runs fn in its own goroutine and releases the overflow
// slot when done.
func (p *Pool) runDetachedTracked(fn func()) {
	go func() {
		defer func() { <-p.overflow }()
		p.run(fn)
	}()
}

// send enqueues fn, recovering the send-on-closed-channel panic from the race
// between the closed check and Shutdown's close. Returns false only when the
// queue is full.
func (p *Pool) send(fn func()) (ok bool) {
	defer func() {
		if recover() != nil {
			p.runDetachedTracked(fn)
			ok = true
		}
	}()
	select {
	case p.ch <- fn:
		return true
	default:
		return false
	}
}

func (p *Pool) runDetached(fn func()) {
	go p.run(fn)
}

func (p *Pool) run(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("worker task panicked", "pool", p.kind, "error", r)
		}
	}()
	fn()
}

func (p *Pool) Shutdown(ctx context.Context) {
	p.closed.Store(true)
	close(p.ch)
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("worker pool shutdown timeout, some tasks may be dropped", "pool", p.kind)
	}
}
