package handler

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crosslink/internal/service"
)

// perRow adapts a per-row flush func to the batch flusher signature for
// tests: the first failing row fails the whole (small) batch, matching the
// all-or-nothing retry granularity of the batch protocol.
func perRow(fn func(*service.UsageEntry) error) func([]*service.UsageEntry) error {
	return func(es []*service.UsageEntry) error {
		for _, e := range es {
			if err := fn(e); err != nil {
				return err
			}
		}
		return nil
	}
}

// DB-ack semantics (supervisor review): a spilled row leaves the spool ONLY
// after flush returns nil; a failing ack keeps the row for the next round.
func TestUsageSpool_AckSemantics(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	if spool == nil {
		t.Fatal("spool not armed")
	}
	entry := &service.UsageEntry{EventID: "x1", RouteType: "openai", ModelRequested: "m", StatusCode: 200}
	if !spool.Write(entry) {
		t.Fatal("write to fresh spool failed")
	}

	// Round 1: DB ack fails — the row must STAY on disk.
	dbDown := true
	calls := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		calls++
		if dbDown {
			return errors.New("db down")
		}
		if e.RouteType != "openai" || e.StatusCode != 200 {
			t.Fatalf("row corrupted through spool: %+v", e)
		}
		return nil
	}), 10)
	if calls != 1 {
		t.Fatalf("replay flush calls = %d, want 1", calls)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "usage-spill.jsonl")); len(data) == 0 {
		t.Fatal("row removed from spool despite failed DB ack")
	}

	// Round 2: DB recovered — ack succeeds and the file empties.
	dbDown = false
	flushed := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		flushed++
		return nil
	}), 10)
	if flushed != 1 {
		t.Fatalf("acked %d rows, want 1", flushed)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "usage-spill.jsonl")); err != nil || len(data) != 0 {
		t.Fatalf("spool file not emptied after ack: %v %d bytes", err, len(data))
	}
}

// Batch granularity (replay rounds flush all-or-nothing): a failed batch
// keeps ALL its rows for retry; rows acked in EARLIER successful rounds are
// removed.
func TestUsageSpool_PartialAckKeepsRemainder(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	if spool == nil {
		t.Fatal("spool not armed")
	}
	spool.Write(&service.UsageEntry{EventID: "a", RouteType: "a"})
	spool.Write(&service.UsageEntry{EventID: "b", RouteType: "b"})
	spool.Write(&service.UsageEntry{EventID: "c", RouteType: "c"})

	// Round 1 (max=1): only 'a' — flushed, acked, stripped.
	if n := spool.Replay(perRow(func(*service.UsageEntry) error { return nil }), 1); n != 1 {
		t.Fatalf("round 1 flushed %d, want 1", n)
	}
	// Round 2: batch {b,c} refused because b fails — BOTH stay on disk.
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		if e.RouteType == "b" {
			return errors.New("db down on b")
		}
		return nil
	}), 10)

	data, _ := os.ReadFile(filepath.Join(dir, "usage-spill.jsonl"))
	content := string(data)
	if strings.Contains(content, `"a"`) {
		t.Fatalf("acked row a still on disk: %q", content)
	}
	if !strings.Contains(content, `"b"`) || !strings.Contains(content, `"c"`) {
		t.Fatalf("unacked rows b/c must remain: %q", content)
	}
}

// The per-round max bounds replay work; rows beyond it stay untouched.
func TestUsageSpool_RoundMaxKeepsRemainder(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	if spool == nil {
		t.Fatal("spool not armed")
	}
	for _, rt := range []string{"a", "b", "c"} {
		if !spool.Write(&service.UsageEntry{EventID: rt, RouteType: rt}) {
			t.Fatal("write failed")
		}
	}
	flushed := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		flushed++
		return nil
	}), 2)
	if flushed != 2 {
		t.Fatalf("flushed %d rows, want round cap 2", flushed)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "usage-spill.jsonl"))
	if !strings.Contains(string(data), `"c"`) {
		t.Fatalf("row c beyond the round cap must remain: %q", data)
	}
}

func TestUsageSpool_CapDrops(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 4096)
	t.Cleanup(spool.Close)
	if spool == nil {
		t.Fatal("spool not armed")
	}
	first := spool.Write(&service.UsageEntry{EventID: "a", RouteType: "a"})
	second := spool.Write(&service.UsageEntry{EventID: "big", RouteType: "b", UserMessage: string(make([]byte, 16<<10))})
	if !first {
		t.Fatal("first write should fit")
	}
	if second {
		t.Fatal("write beyond cap should be refused")
	}
}

// A torn final line (crash mid-append, no newline) must be truncated on
// startup so it cannot concatenate with — and corrupt — the next appended row.
func TestUsageSpool_TornTailTruncatedOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-spill.jsonl")
	os.WriteFile(path, []byte("{\"route_ty"), 0o644)

	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	if spool == nil {
		t.Fatal("spool not armed")
	}
	if !spool.Write(&service.UsageEntry{EventID: "good", RouteType: "good"}) {
		t.Fatal("write after torn tail failed")
	}
	seen := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		seen++
		if e.RouteType != "good" {
			t.Fatalf("row after torn tail corrupted: %+v", e)
		}
		return nil
	}), 10)
	if seen != 1 {
		t.Fatalf("replay saw %d rows, want 1 (torn tail must not swallow the next row)", seen)
	}
}
