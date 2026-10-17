package handler

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/service"
)

// ---------------------------------------------------------------------------
// Durability-watermark barrier tests (independent review blocker, 2026-10-17):
// an fsync confirms ONLY the appends that were in the file when it started.
// A Write that appends while an fsync is in flight must NOT be released by
// that fsync — it must wait for the NEXT successful one.
// ---------------------------------------------------------------------------

// gatedSync turns the walSync seam into a two-stage controllable barrier:
// fsync #1 and #2 each signal their start and block until their release
// channel closes; later calls pass straight through.
func gatedSync(t *testing.T) (started1, release1, started2, release2 chan struct{}, calls *atomic.Int32) {
	t.Helper()
	started1 = make(chan struct{}, 1)
	release1 = make(chan struct{})
	started2 = make(chan struct{}, 1)
	release2 = make(chan struct{})
	calls = &atomic.Int32{}
	prev := walSync
	walSync = func(f *os.File) error {
		switch calls.Add(1) {
		case 1:
			started1 <- struct{}{}
			<-release1
		case 2:
			started2 <- struct{}{}
			<-release2
		}
		return nil
	}
	t.Cleanup(func() { walSync = prev })
	return
}

// Core barrier: W2 appends AFTER the first fsync started but BEFORE its
// confirmation lands. W2 must still be blocked when that fsync returns, and
// only the SECOND fsync may release it. (Pre-fix, syncLoop read appSeq after
// the fsync and falsely confirmed W2 — this test fails on that code.)
func TestGroupCommit_WatermarkBarrier(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 8<<20)
	t.Cleanup(spool.Close)
	started1, release1, started2, release2, calls := gatedSync(t)

	// W1: appends, wakes the syncer, which captures target=1 and enters the
	// (gated) fsync #1.
	w1done := make(chan bool, 1)
	go func() { w1done <- spool.Write(&service.UsageEntry{EventID: "w1", RouteType: "openai"}) }()
	<-started1 // syncer is inside fsync #1 now

	// W2: appends DURING that fsync (after target capture). It must block.
	w2done := make(chan bool, 1)
	go func() { w2done <- spool.Write(&service.UsageEntry{EventID: "w2", RouteType: "openai"}) }()
	time.Sleep(150 * time.Millisecond) // let W2 append + start waiting

	select {
	case <-w2done:
		t.Fatal("W2 released while the covering fsync is still gated — impossible without a false confirmation")
	default:
	}

	// Release fsync #1: it covers ONLY seq 1 (W1). W1 completes; W2 must
	// STILL be blocked — the old fsync must not confirm it.
	close(release1)
	if ok := <-w1done; !ok {
		t.Fatal("W1 must be confirmed by the fsync that covered its append")
	}
	select {
	case <-w2done:
		t.Fatal("W2 falsely confirmed by the PRE-W2 fsync (watermark read after fsync — the review blocker)")
	case <-time.After(300 * time.Millisecond):
	}

	// The syncer's fallback tick starts fsync #2 (covering W2) — gated. W2
	// must STILL be blocked while fsync #2 is in flight.
	select {
	case <-started2:
	case <-time.After(5 * time.Second):
		t.Fatalf("second fsync never started (calls=%d)", calls.Load())
	}
	select {
	case <-w2done:
		t.Fatal("W2 released before the SECOND fsync completed — barrier broken")
	case <-time.After(300 * time.Millisecond):
	}

	// Only completing fsync #2 releases W2.
	close(release2)
	select {
	case ok := <-w2done:
		if !ok {
			t.Fatal("W2 must be confirmed by the second fsync")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("W2 never released after the covering fsync completed")
	}
}

// Rename concurrency: a Compact that swaps the append handle while an fsync
// is in flight must not strand the writer — the event is confirmed either by
// Compact's atomic-replace advance (tmp was fsynced) or by the next sync.
func TestGroupCommit_RenameDuringInflightSync(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 8<<20)
	t.Cleanup(spool.Close)
	// Ack something so Compact has work; pre-seed one acked line.
	if !spool.Write(&service.UsageEntry{EventID: "seed", RouteType: "openai"}) {
		t.Fatal("seed write failed")
	}
	spool.Ack("seed")

	started1, release1, started2, release2, _ := gatedSync(t)
	// Auto-release the second gate when it starts: this test only needs the
	// FIRST fsync gated; later fsyncs must run freely to confirm the writer.
	go func() { <-started2; close(release2) }()
	wdone := make(chan bool, 1)
	go func() { wdone <- spool.Write(&service.UsageEntry{EventID: "rn-1", RouteType: "openai"}) }()
	<-started1 // syncer inside gated fsync #1

	// Run a Compact concurrently (it takes mu, swaps the handle).
	go func() { spool.Compact() }()
	time.Sleep(150 * time.Millisecond)
	close(release1)

	select {
	case ok := <-wdone:
		if !ok {
			t.Fatal("writer reported non-durable despite Compact/rename durability path")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("writer stranded by concurrent rename (no confirmation path advanced the watermark)")
	}

	// The event must survive on disk exactly once and replay once.
	spool2 := NewUsageSpool(dir, 8<<20)
	t.Cleanup(spool2.Close)
	count := 0
	for {
		n := spool2.Replay(perRow(func(e *service.UsageEntry) error {
			if e.EventID == "rn-1" {
				count++
			}
			return nil
		}), 10000)
		if n == 0 {
			break
		}
	}
	if count != 1 {
		t.Fatalf("rn-1 recovered %d times, want exactly 1", count)
	}
}

// Watermark monotonicity: a late fsync covering an older target must never
// pull syncedSeq backwards (e.g. after Compact advanced it).
func TestGroupCommit_WatermarkNeverRegresses(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 8<<20)
	t.Cleanup(spool.Close)

	if !spool.Write(&service.UsageEntry{EventID: "m1", RouteType: "openai"}) {
		t.Fatal("write failed")
	}
	spool.syncMu.Lock()
	before := spool.syncedSeq
	spool.syncMu.Unlock()
	if before < 1 {
		t.Fatalf("syncedSeq = %d after a confirmed write", before)
	}
	spool.confirmSynced(0) // a stale/old target late in flight
	spool.syncMu.Lock()
	after := spool.syncedSeq
	spool.syncMu.Unlock()
	if after != before {
		t.Fatalf("watermark regressed: %d -> %d", before, after)
	}
}
