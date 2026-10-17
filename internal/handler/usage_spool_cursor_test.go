package handler

import (
	"testing"
	"time"

	"github.com/crosslink/internal/service"
)

// DRAIN CURSOR regression: when compaction is deferred (big file, halving
// threshold not met, fresh staleness clock), replay rounds must still ADVANCE
// past acked-but-unstripped lines via the byte cursor — pre-cursor, every
// round re-read the same front lines, re-flushed them idempotently, and the
// drain only moved when the 30s staleness strip fired (observed live: exactly
// one 200-row round per 30s tick on a 158k-line backlog).
func TestReplay_CursorAdvancesPastAckedUnstrippedLines(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 64<<20)
	t.Cleanup(spool.Close)
	const total = 6000 // > the 4096-line small-file gate, so compaction defers
	for i := 0; i < total; i++ {
		e := service.UsageEntry{EventID: fmtGateID("c", i), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}
	// Freeze the compaction gate: big file, fresh clock, sub-half acks.
	spool.ackMu.Lock()
	spool.lastCompactAt = time.Now()
	spool.ackMu.Unlock()

	var calls int
	flush := perRow(func(*service.UsageEntry) error { calls++; return nil })
	// Two back-to-back rounds with NO compaction in between (deferred).
	n1 := spool.Replay(flush, 200)
	n2 := spool.Replay(flush, 200)
	if n1 != 200 || n2 != 200 {
		t.Fatalf("rounds flushed %d,%d; want 200,200", n1, n2)
	}
	if calls != 400 {
		t.Fatalf("flush calls = %d, want 400 — round 2 re-flushed acked lines (cursor not advancing)", calls)
	}
	if spool.replayCursor <= 0 {
		t.Fatalf("cursor = %d, want > 0", spool.replayCursor)
	}

	// After a successful Compact the cursor resets to the (new) front, and a
	// fresh round must NOT re-flush anything already acked.
	spool.Compact()
	calls = 0
	if n := spool.Replay(flush, 200); n != 200 {
		t.Fatalf("post-compact round flushed %d, want 200 (next UNacked batch)", n)
	}
	if calls != 200 {
		t.Fatalf("post-compact flush calls = %d, want 200", calls)
	}
}

// A failing batch parks the cursor AT the batch's first line: the retry round
// must resume from exactly that row (all-or-nothing per batch).
func TestReplay_CursorParksAtFailedBatch(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	for i := 0; i < 5; i++ {
		e := service.UsageEntry{EventID: string(rune('a' + i)), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}
	var calls int
	n := spool.Replay(perRow(func(e *service.UsageEntry) error {
		calls++
		if e.EventID >= "c" {
			return errFake
		}
		return nil
	}), 10)
	if n != 0 || calls != 3 {
		t.Fatalf("failed batch flushed=%d calls=%d; want 0,3 (batch refused, nothing acked)", n, calls)
	}
	if spool.replayCursor != 0 {
		t.Fatalf("cursor = %d after refused batch, want 0 (parked at batch start)", spool.replayCursor)
	}
	calls = 0
	n = spool.Replay(perRow(func(*service.UsageEntry) error { calls++; return nil }), 10)
	if n != 5 || calls != 5 {
		t.Fatalf("retry round flushed=%d calls=%d; want 5,5 (resume at batch start)", n, calls)
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake db failure" }
