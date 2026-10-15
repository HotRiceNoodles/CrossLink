package handler

import (
	"os"
	"testing"
	"time"

	"github.com/crosslink/internal/service"
)

// CompactIfWorthwhile amortization contract:
//   - small files (<=4096 lines): compacts whenever anything is acked —
//     steady-state behavior is unchanged (fast strip, cheap rewrite);
//   - large files: a rewrite runs only when it would strip >= half the lines,
//     or after the 30s staleness bound — never one full rewrite per replay
//     round (the soak-r2 failure: 158k lines drained at 6.7 rows/s because
//     every 200-row round paid a full O(file) rewrite);
//   - deferral never loses data: acks stay in memory, lines stay on disk.
func TestCompactIfWorthwhile_SmallFileCompacts(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	for i := 0; i < 10; i++ {
		e := service.UsageEntry{EventID: string(rune('a' + i)), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}
	spool.Replay(func(*service.UsageEntry) error { return nil }, 10) // flushes + acks
	spool.CompactIfWorthwhile()
	if p := spool.Pending(); p != 0 {
		t.Fatalf("small file must compact immediately, pending=%d", p)
	}
}

func TestCompactIfWorthwhile_LargeFileDefersUntilHalvingOrStale(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 64<<20)
	for i := 0; i < 10000; i++ {
		e := service.UsageEntry{EventID: fmtGateID("e", i), RouteType: "openai", StatusCode: 200}
		if !spool.Write(&e) {
			t.Fatal("write failed")
		}
	}
	// Simulate one drain round's acks: 200 acks on a 10000-line file (< half)
	// with a FRESH staleness clock must NOT rewrite.
	for i := 0; i < 200; i++ {
		spool.Ack(fmtGateID("e", i))
	}
	spool.ackMu.Lock()
	spool.lastCompactAt = time.Now()
	spool.ackMu.Unlock()
	spool.CompactIfWorthwhile()
	if p := spool.Pending(); p != 10000 {
		t.Fatalf("large file must defer the rewrite (200/<half acks, fresh clock), pending=%d", p)
	}
	if _, err := os.Stat(dir + string(os.PathSeparator) + "usage-spill.jsonl.tmp"); !os.IsNotExist(err) {
		t.Fatal("no tmp rewrite should have been attempted")
	}

	// Halving threshold reached: the rewrite must run.
	for i := 200; i < 5100; i++ {
		spool.Ack(fmtGateID("e", i))
	}
	spool.CompactIfWorthwhile()
	if p := spool.Pending(); p != 4900 {
		t.Fatalf("halving acks must compact, pending=%d want 4900", p)
	}

	// Staleness bound: a small ack batch on the big file compacts after 30s+.
	for i := 5100; i < 5300; i++ {
		spool.Ack(fmtGateID("e", i))
	}
	spool.ackMu.Lock()
	spool.lastCompactAt = time.Now().Add(-31 * time.Second)
	spool.ackMu.Unlock()
	spool.CompactIfWorthwhile()
	if p := spool.Pending(); p != 4700 {
		t.Fatalf("stale clock must allow compaction, pending=%d want 4700", p)
	}
}

func fmtGateID(prefix string, i int) string {
	digits := []rune("0123456789abcdefghijklmnopqrstuvwxyz")
	id := []rune(prefix)
	for _, div := range []int{1296, 36, 1} {
		id = append(id, digits[(i/div)%36])
	}
	return string(id)
}
