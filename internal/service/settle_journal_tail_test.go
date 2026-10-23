package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// Journal-tail durability tests (review 2026-10-22 round 8, task 2). The
// journal write path is O_APPEND write with NO per-line fsync: the SUPPORTED
// crash class is process kill (kernel owns the page cache thereafter).
// Power loss can leave (a) a truncated final record (write torn mid-line)
// or (b) an entirely unwritten tail (bytes still in page cache, lost).
// These tests pin detection, budget state, and the recovery gate for both
// shapes — using file-level simulation only, never a host power event.

// T27: torn final line (no trailing newline, cut mid-JSON). The replay
// must drop exactly that line (auditable metric), recover every complete
// line, and not wedge the boot gate.
func TestSettleJournal_T27_TornTailLine(t *testing.T) {
	s, cal, rdb, path := newJournalHarness(t)
	appendJournalLine(t, path, "key", "71", "t27-a", 2.0, time.Now().Unix())
	appendJournalLine(t, path, "key", "71", "t27-b", 3.0, time.Now().Unix())
	// Torn tail: first 17 bytes of a valid record, no newline.
	full, _ := os.ReadFile(path)
	torn := append(full, []byte(`{"scope":"key","target":"71","per`)...)
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if err != nil {
		t.Fatalf("T27 round must succeed on a torn tail (boot gate cannot wedge): %v", err)
	}
	if st.Total != 2 || st.Recovered != 2 || st.Expired != 0 {
		t.Fatalf("T27 total=%d recovered=%d expired=%d, want 2/2/0 (torn line is NOT a recovered intent)", st.Total, st.Recovered, st.Expired)
	}
	settled, _ := rdb.Get(context.Background(), "budgetset:key:71:2026-10").Float64()
	if settled != 5.0 {
		t.Fatalf("T27 settled=%v want 5.0 (complete lines only)", settled)
	}
	if n := countJournalLines(t, path); n != 0 {
		t.Fatalf("T27 journal kept %d lines after replay", n)
	}
}

// T28: garbage line mid-file (corruption, not a torn tail). Same policy:
// drop auditable, replay the rest, round succeeds.
func TestSettleJournal_T28_MidFileGarbage(t *testing.T) {
	s, cal, rdb, path := newJournalHarness(t)
	now := time.Now().Unix()
	appendJournalLine(t, path, "key", "72", "t28-a", 1.0, now)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not-json-at-all\n\x00\x01garbage\n")
	f.Close()
	appendJournalLine(t, path, "key", "72", "t28-b", 4.0, now)
	st, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if err != nil {
		t.Fatalf("T28 round: %v", err)
	}
	if st.Total != 2 || st.Recovered != 2 {
		t.Fatalf("T28 total=%d recovered=%d, want 2/2 (garbage lines dropped)", st.Total, st.Recovered)
	}
	settled, _ := rdb.Get(context.Background(), "budgetset:key:72:2026-10").Float64()
	if settled != 5.0 {
		t.Fatalf("T28 settled=%v want 5.0", settled)
	}
}

// T29: UNWRITTEN tail — the record was never persisted (page-cache loss
// shape). The journal simply lacks the line; its usage row IS in the DB.
// Convergence must come from calibration exactly once (near segment,
// state-aware + mark), never from a phantom journal recovery.
func TestSettleJournal_T29_UnwrittenTailHealsViaCalibration(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}, &model.BudgetSnapshot{}); err != nil {
		t.Fatal(err)
	}
	s := &BudgetService{rdb: rdb, markerTTL: time.Minute}
	if err := s.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cal := NewBudgetCalibrationService(db, rdb)
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })

	// The intent WAS appended, then the tail never made it to the file
	// (simulate by NOT appending): only the DB row exists.
	tid := int64(73)
	tm := tid
	row := model.UsageLog{RequestID: "t29-lost", TeamID: &tm, Cost: 6.0,
		Currency: "CNY", RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	// Boot gate replays an EMPTY journal: nothing recovered...
	if _, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second); err != nil {
		t.Fatalf("T29 empty round: %v", err)
	}
	b0, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:2026-10", tid)).Float64()
	if b0 != 0 {
		t.Fatalf("T29 pre-calibration budget=%v want 0 (journal never had the line)", b0)
	}
	// ...and the calibration converges the row exactly once.
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("T29 lock")
	}
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, "2026-10", "monthly", start, end); err != nil {
		t.Fatalf("T29 calibrate: %v", err)
	}
	rel()
	b1, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:2026-10", tid)).Float64()
	if b1 != 6.0 {
		t.Fatalf("T29 post-calibration budget=%v want 6.0 (unwritten tail heals via DB, exactly once)", b1)
	}
	if rdb.Exists(context.Background(), "settledmark:team:73:t29-lost").Val() != 1 {
		t.Fatalf("T29 healed row must carry a settledmark (idempotent against any late owner)")
	}
	// A late journal line for the SAME row (re-discovered tail) must no-op.
	appendJournalLine(t, s.journal.path, "team", "73", "t29-lost", 6.0, time.Now().Unix())
	st, rerr := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if rerr != nil {
		t.Fatalf("T29 late round: %v", rerr)
	}
	if st.Recovered != 0 {
		t.Fatalf("T29 late round recovered=%d want 0 (mark shields the re-found line)", st.Recovered)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:2026-10", tid)).Float64()
	if b2 != 6.0 {
		t.Fatalf("T29 final budget=%v want 6.0", b2)
	}
}
