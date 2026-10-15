package handler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/service"
)

// Replay's RETURN VALUE is the drain loop's progress signal: Pending() counts
// concurrent new arrivals too, so a Pending()-based "no progress" check breaks
// the drain after one round per tick whenever live traffic outpaces one round
// (observed live: a 160k-line backlog drained at 6.7 rows/s — one 200-row
// round per 30s tick — while soak traffic added ~33 rows/s; projected recovery
// ~13h). The contract: Replay reports exactly the rows the DB accepted this
// round, independent of arrivals.
func TestReplay_ReturnsFlushedCount(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	for i := 0; i < 5; i++ {
		e := service.UsageEntry{EventID: string(rune('a' + i)), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}

	if n := spool.Replay(func(*service.UsageEntry) error { return nil }, 10); n != 5 {
		t.Fatalf("full round flushed = %d, want 5", n)
	}
	if n := spool.Replay(func(*service.UsageEntry) error { return nil }, 10); n != 0 {
		t.Fatalf("empty spool flushed = %d, want 0", n)
	}
}

// Mid-batch DB failure must report only the accepted prefix (the drain loop
// treats 0 as "DB refusing rows" and stops; a partial count keeps it going on
// the next round — never a false stop, never an infinite retry of dead rows).
func TestReplay_ReturnsPartialCountOnMidBatchFailure(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	for i := 0; i < 4; i++ {
		e := service.UsageEntry{EventID: string(rune('a' + i)), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}
	n := spool.Replay(func(e *service.UsageEntry) error {
		if e.EventID >= "c" {
			return errors.New("db down mid-batch")
		}
		return nil
	}, 10)
	if n != 2 {
		t.Fatalf("partial round flushed = %d, want 2", n)
	}
}

// End-to-end drain-mode regression: with live arrivals landing BETWEEN rounds
// at a rate above one round per tick, the loop must keep draining (the old
// Pending()-based break stalled at one round per tick). Pass condition: the
// FULL backlog drains while arrivals continue — impossible at one round/tick
// because the backlog (300) exceeds one round (200).
func TestReplay_DrainLoopProgressesUnderArrivals(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 8<<20)
	const backlog = 300
	for i := 0; i < backlog; i++ {
		e := service.UsageEntry{EventID: fmtDrainID("backlog-", i), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}

	var flushed atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go RunUsageSpoolReplay(ctx, spool, 500*time.Millisecond, func(_ context.Context, _ *service.UsageEntry) error {
		flushed.Add(1)
		return nil
	})

	// Live arrivals: written straight to the spool (bypassing workers) as
	// fast as fsync allows — well above one round per 500ms tick (400 rows/s).
	arrivals := 0
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		e := service.UsageEntry{EventID: fmtDrainID("live-", arrivals), RouteType: "openai", StatusCode: 200}
		if spool.Write(&e) {
			arrivals++
		}
	}

	// Old behavior: ≤1 round per tick → ≤ 2 rounds before arrivals end
	// (~400 rows). New: backlog + arrivals all flush. Backlog alone forces
	// ≥2 rounds in a single tick.
	if n := flushed.Load(); n < backlog {
		t.Fatalf("drain starved under live arrivals: flushed=%d < backlog=%d (old Pending()-based break stalls at one round/tick)", n, backlog)
	}

	// Settle: stop arrivals, let the loop drain the rest, then a final full
	// round must leave nothing pending.
	time.Sleep(700 * time.Millisecond)
	spool.Replay(func(*service.UsageEntry) error { return nil }, 100000)
	spool.Compact()
	if p := spool.Pending(); p != 0 {
		t.Fatalf("pending after settled drain = %d, want 0", p)
	}
}

// fmtDrainID builds short unique EventIDs (unique up to 26^3*10).
func fmtDrainID(prefix string, i int) string {
	return prefix + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)) + string(rune('0'+i%10))
}
