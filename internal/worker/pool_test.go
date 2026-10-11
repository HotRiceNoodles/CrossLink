package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPool_SubmitExecutesTasks(t *testing.T) {
	p := NewPool("test", 2, 8, 4)
	var ran atomic.Int64
	done := make(chan struct{})
	p.Submit(func() {
		ran.Add(1)
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Shutdown(ctx)
	if ran.Load() != 1 {
		t.Fatalf("expected 1 task run, got %d", ran.Load())
	}
}

// TestPool_SubmitAfterShutdownDoesNotPanic locks in the fix for the
// send-on-closed-channel panic: tasks submitted after Shutdown must run
// detached (best-effort flush) instead of panicking.
func TestPool_SubmitAfterShutdownDoesNotPanic(t *testing.T) {
	p := NewPool("test", 1, 4, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Shutdown(ctx)

	ran := make(chan struct{})
	p.Submit(func() { close(ran) }) // used to panic here
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("post-shutdown task did not run")
	}
}

// TestPool_QueueFullRunsViaOverflow verifies a full queue degrades to bounded
// detached execution instead of dropping the task.
func TestPool_QueueFullRunsViaOverflow(t *testing.T) {
	// 1 worker, queue depth 1, overflow 2.
	p := NewPool("test", 1, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer p.Shutdown(ctx)

	// Occupy the worker, waiting for proof it has STARTED before filling the
	// queue — otherwise the filler submit may take the overflow path (queue
	// still holds task 1) and the queue slot frees up for the probe task,
	// which then waits forever behind the blocked worker.
	started := make(chan struct{})
	block := make(chan struct{})
	p.Submit(func() {
		close(started)
		<-block
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first task never started; worker not running")
	}

	p.Submit(func() {}) // fills the queue (worker provably occupied)

	// Queue full — this must run via the overflow allowance, not drop.
	ran := make(chan struct{})
	p.Submit(func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("task submitted to a full queue did not run (dropped instead of overflowing)")
	}
	if got := p.Dropped(); got != 0 {
		t.Fatalf("overflowed task must not be counted as dropped, got %d", got)
	}
	close(block)
}

// TestPool_OverflowExhaustedDrops verifies the drop counter only moves once
// both the queue and the overflow allowance are full.
func TestPool_OverflowExhaustedDrops(t *testing.T) {
	// 1 worker, queue depth 1, overflow 0 → the first extra task drops.
	p := NewPool("test", 1, 1, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer p.Shutdown(ctx)

	// Occupy the worker and wait until it is actually running the task, so
	// the queue state is deterministic before the probe.
	started := make(chan struct{})
	block := make(chan struct{})
	p.Submit(func() {
		close(started)
		<-block
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never picked up the blocking task")
	}
	p.Submit(func() {}) // fills the queue
	p.Submit(func() { t.Error("dropped task must not run") })
	if got := p.Dropped(); got != 1 {
		t.Fatalf("expected 1 dropped task, got %d", got)
	}
	close(block)
}
