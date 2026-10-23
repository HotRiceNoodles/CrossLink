package service

// T17-T22 (review 2026-10-22: the usage-scan approach is replaced by the
// durable settle journal; these pin the demanded edges):
//   T17 >20000 lines stream (no DB Limit, no missed tail);
//   T18 mark-TTL class: lines past the settle guard window EXPIRE loudly
//       instead of being re-applied against possibly-absent marks;
//   T19 Redis read failure during replay keeps lines (never guessed);
//   T20 lock mutual exclusion: replay defers when a calibrator holds the
//       calibration lock (no concurrent raise);
//   T21 PG independence: replay never consults the DB (WAL-late rows are
//       irrelevant to the journal's recovery decision);
//   T22 idempotence + exact counters (second replay no-ops).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

func newJournalHarness(t *testing.T) (*BudgetService, *BudgetCalibrationService, *redis.Client, string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := &BudgetService{rdb: rdb, markerTTL: time.Minute} // guard ~= 1m + 10m + 1h
	if err := s.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatalf("journal open: %v", err)
	}
	t.Cleanup(s.CloseSettleJournal)
	cal := NewBudgetCalibrationService(nil, nil)
	cal.rdb = rdb
	return s, cal, rdb, s.journal.path
}

func appendJournalLine(t *testing.T, path, scope, target, reqID string, cost float64, ts int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	defer f.Close()
	b, _ := json.Marshal(journalLine{Scope: scope, Target: target, Period: "monthly", Pk: "2026-10",
		ReqID: reqID, Cost: cost, Ts: ts, Version: 1})
	f.Write(append(b, '\n'))
}

// T17: 25,000 lines (past the old scan's 20,000 Limit) all processed.
func TestSettleJournal_T17_StreamsPastTwentyThousand(t *testing.T) {
	s, cal, rdb, path := newJournalHarness(t)
	now := time.Now().Unix()
	var sb strings.Builder
	for i := 0; i < 25000; i++ {
		b, _ := json.Marshal(journalLine{Scope: "key", Target: "7", Period: "monthly", Pk: "2026-10",
			ReqID: fmt.Sprintf("id-%05d", i), Cost: 0.001, Ts: now, Version: 1})
		sb.Write(append(b, '\n'))
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if st.Total != 25000 || st.Recovered != 25000 || st.KeptForRetry != 0 {
		t.Fatalf("T17: total=%d recovered=%d kept=%d, want 25000/25000/0", st.Total, st.Recovered, st.KeptForRetry)
	}
	settled, _ := rdb.Get(context.Background(), "budgetset:key:7:2026-10").Float64()
	if d := settled - 25.0; d < -1e-6 || d > 1e-6 {
		t.Fatalf("T17 settled=%v want 25.0 (25000x0.001)", settled)
	}
	if n := countJournalLines(t, path); n != 0 {
		t.Fatalf("T17 journal kept %d lines after full replay", n)
	}
}

// T18: a line older than the guard window (its settledmark has expired)
// must EXPIRE (calibration owns the heal), never re-apply.
func TestSettleJournal_T18_GuardWindowExpiry(t *testing.T) {
	s, cal, rdb, path := newJournalHarness(t)
	old := time.Now().Add(-3 * time.Hour).Unix() // guard ~= 71m with markerTTL=1m
	appendJournalLine(t, path, "key", "8", "old-1", 5.0, old)
	appendJournalLine(t, path, "key", "8", "new-1", 2.0, time.Now().Unix())
	st, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if st.Expired != 1 || st.Recovered != 1 {
		t.Fatalf("T18: expired=%d recovered=%d, want 1/1", st.Expired, st.Recovered)
	}
	settled, _ := rdb.Get(context.Background(), "budgetset:key:8:2026-10").Float64()
	if d := settled - 2.0; d < -1e-9 || d > 1e-9 {
		t.Fatalf("T18 settled=%v want exactly 2.0 (expired line NOT applied)", settled)
	}
}

// T19: a Redis read failure keeps every unresolved line — the error is
// never interpreted as mark-absent (double-apply) or mark-present (loss).
func TestSettleJournal_T19_RedisReadErrorKeepsLines(t *testing.T) {
	s, cal, _, path := newJournalHarness(t)
	appendJournalLine(t, path, "key", "9", "keep-1", 1.0, time.Now().Unix())
	appendJournalLine(t, path, "key", "9", "keep-2", 1.0, time.Now().Unix())
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { dead.Close() })
	s.rdb = dead
	cal.rdb = dead
	st, err := s.SettleJournalRound(context.Background(), cal, 2*time.Second)
	if err == nil {
		t.Fatalf("T19: expected lock-store error, got stats %+v", st)
	}
	if n := countJournalLinesAll(t, path); n != 2 {
		t.Fatalf("T19 journal holds %d lines, want 2 (kept verbatim)", n)
	}
}

// T20: replay defers (loud error) while a calibrator holds the lock.
func TestSettleJournal_T20_LockMutualExclusion(t *testing.T) {
	s, cal, _, path := newJournalHarness(t)
	appendJournalLine(t, path, "key", "10", "race-1", 1.0, time.Now().Unix())
	_, release, err := cal.acquireCalibrationLock(context.Background())
	if err != nil || release == nil {
		t.Fatalf("calibrator could not take the lock: %v (release nil: %v)", err, release == nil)
	}
	_, jerr := s.SettleJournalRound(context.Background(), cal, 300*time.Millisecond)
	if jerr == nil || !strings.Contains(jerr.Error(), "lock not acquired") {
		t.Fatalf("T20: expected lock-not-acquired, got %v", jerr)
	}
	if n := countJournalLinesAll(t, path); n != 1 {
		t.Fatalf("T20 journal must be untouched while lock held, has %d", n)
	}
	release()
	st, err := s.SettleJournalRound(context.Background(), cal, 2*time.Second)
	if err != nil || st.Recovered != 1 {
		t.Fatalf("T20 post-release round: %+v %v", st, err)
	}
}

// T21: replay works with a nil DB handle — the journal's recovery decision
// never consults usage_logs (WAL-late rows cannot change the outcome).
func TestSettleJournal_T21_NoDBDependency(t *testing.T) {
	s, cal, rdb, path := newJournalHarness(t)
	// cal.db is nil in this harness already.
	appendJournalLine(t, path, "team", "11", "nodb-1", 3.0, time.Now().Unix())
	st, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if err != nil || st.Recovered != 1 {
		t.Fatalf("T21: %+v %v", st, err)
	}
	v, _ := rdb.Get(context.Background(), "budgetset:team:11:2026-10").Float64()
	if v != 3.0 {
		t.Fatalf("T21 settled=%v want 3.0", v)
	}
}

// T22: idempotence — re-append of an already-settled line is a pure no-op
// and counters stay exactly once.
func TestSettleJournal_T22_Idempotent(t *testing.T) {
	s, cal, rdb, path := newJournalHarness(t)
	appendJournalLine(t, path, "org", "12", "idem-1", 0.5, time.Now().Unix())
	for i := 0; i < 2; i++ {
		if _, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		appendJournalLine(t, path, "org", "12", "idem-1", 0.5, time.Now().Unix())
	}
	v, _ := rdb.Get(context.Background(), "budgetset:org:12:2026-10").Float64()
	if v != 0.5 {
		t.Fatalf("T22 settled=%v want 0.5 (exactly once)", v)
	}
}

func countJournalLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

func countJournalLinesAll(t *testing.T, path string) int {
	t.Helper()
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || e.Name() != journalActiveName && !strings.HasSuffix(e.Name(), journalSealSuffix) {
			continue
		}
		f, ferr := os.Open(filepath.Join(dir, e.Name()))
		if ferr != nil {
			t.Fatalf("open %s: %v", e.Name(), ferr)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) != "" {
				n++
			}
		}
		f.Close()
	}
	return n
}

// T23 (v2 review counterexample 1): concurrent appends during a replay
// round must never be lost — sealing rotates the active file and appends
// continue into the fresh one; every intent settles exactly once.
func TestSettleJournal_T23_ConcurrentAppendDuringReplayNoLoss(t *testing.T) {
	s, cal, rdb, _ := newJournalHarness(t)
	for i := 0; i < 300; i++ {
		if err := s.journal.append(settleRetryTask{scope: "key", targetID: "20", period: "monthly", pk: "2026-10", reqID: fmt.Sprintf("pre-%03d", i), cost: 0.001}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			_ = s.journal.append(settleRetryTask{scope: "key", targetID: "20", period: "monthly", pk: "2026-10", reqID: fmt.Sprintf("live-%03d", i), cost: 0.001})
			time.Sleep(200 * time.Microsecond)
		}
	}()
	if _, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	<-done
	if _, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if n := countJournalLinesAll(t, s.journal.path); n != 0 {
		t.Fatalf("T23 journal still holds %d lines (lost or undrained)", n)
	}
	settled, _ := rdb.Get(context.Background(), "budgetset:key:20:2026-10").Float64()
	if d := settled - 0.8; d < -1e-6 || d > 1e-6 {
		t.Fatalf("T23 settled=%v want 0.8 (800 x 0.001, none lost none doubled)", settled)
	}
}

// T24 (v2/v3 review counterexample 3): a raise that already absorbed a
// usage row must CERTIFY it ATOMICALLY with the counter update (one
// script: raise-to-cumulative + mark the page's rows), so a later journal
// replay of the same intent is a no-op — and no crash instant exists where
// a row is counted-unmarked or marked-uncounted.
func TestSettleJournal_T24_RaiseCertificationBlocksDoubleApply(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatal(err)
	}
	tid := int64(21)
	if err := db.Create(&model.UsageLog{RequestID: "absorbed-1", TeamID: &tid, Cost: 2.0, Currency: "CNY",
		RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	cal := NewBudgetCalibrationService(db, rdb)
	start, end := periodBoundaries("monthly")
	tok24, rel24, _ := cal.acquireCalibrationLock(context.Background())
	if rel24 == nil {
		t.Fatal("T24 lock")
	}
	spent, cerr := cal.calibrateTarget(context.Background(), tok24, "team", 21, "2026-10", "monthly", start, end)
	rel24() // release BEFORE the journal round needs the same lock
	if cerr != nil {
		t.Fatalf("T24 calibrateTarget: %v", cerr)
	}
	if spent != 2.0 {
		t.Fatalf("T24 spent=%v want 2.0 (same-snapshot page sum)", spent)
	}
	if rdb.Exists(context.Background(), settledMarkKey("team", "21", "absorbed-1")).Val() != 1 {
		t.Fatal("T24 atomic page did not certify the absorbed row")
	}
	budget, _ := rdb.Get(context.Background(), "budget:team:21:2026-10").Float64()
	if budget != 2.0 {
		t.Fatalf("T24 budget=%v want 2.0 (raise and mark in one invocation)", budget)
	}

	s := &BudgetService{rdb: rdb, markerTTL: time.Minute}
	if err := s.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSettleJournal)
	appendJournalLine(t, s.journal.path, "team", "21", "absorbed-1", 2.0, time.Now().Unix())
	st, rerr := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if rerr != nil {
		t.Fatalf("T24 round: %v", rerr)
	}
	if st.AlreadyMarked != 1 || st.Recovered != 0 {
		t.Fatalf("T24 stats=%+v, want already=1 recovered=0 (certified row NOT re-applied)", st)
	}
	budget2, _ := rdb.Get(context.Background(), "budget:team:21:2026-10").Float64()
	if budget2 != 2.0 {
		t.Fatalf("T24 budget after replay=%v want 2.0 (replay added nothing)", budget2)
	}
}

// T25 (v2 review, lease safety): ownership verification between batches —
// a lease lost mid-round aborts the segment with lines KEPT and the round
// reports incomplete so the gate retries.
func TestSettleJournal_T25_LeaseLostAbortsKept(t *testing.T) {
	s, cal, rdb, _ := newJournalHarness(t)
	for i := 0; i < 3; i++ {
		appendJournalLine(t, s.journal.path, "key", "22", fmt.Sprintf("lease-%d", i), 1.0, time.Now().Unix())
	}
	corrupted := false
	journalBatchProbe = func() {
		if !corrupted {
			corrupted = true
			rdb.Set(context.Background(), calibrationLockKey, "other-holder", time.Hour)
		}
	}
	t.Cleanup(func() { journalBatchProbe = nil })
	_, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "kept for retry") {
		t.Fatalf("T25 expected incomplete-round error, got %v", err)
	}
	if n := countJournalLinesAll(t, s.journal.path); n != 3 {
		t.Fatalf("T25 lines after lease loss = %d, want 3 (kept verbatim)", n)
	}
	journalBatchProbe = nil
	// The fake holder releases (test simulates its eventual unlock).
	rdb.Del(context.Background(), calibrationLockKey)
	if _, err := s.SettleJournalRound(context.Background(), cal, 5*time.Second); err != nil {
		t.Fatalf("T25 second round: %v", err)
	}
	settled, _ := rdb.Get(context.Background(), "budgetset:key:22:2026-10").Float64()
	if settled != 3.0 {
		t.Fatalf("T25 settled=%v want 3.0 after retry", settled)
	}
}

// T26 (v2 review, strict gate): the boot gate does NOT pass while the lock
// is unavailable and completes once it is.
func TestSettleJournal_T26_BootGateWaitsForLock(t *testing.T) {
	s, cal, _, _ := newJournalHarness(t)
	appendJournalLine(t, s.journal.path, "key", "23", "gate-1", 1.0, time.Now().Unix())
	_, release, err := cal.acquireCalibrationLock(context.Background())
	if err != nil || release == nil {
		t.Fatalf("take lock: %v", err)
	}
	gateErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go func() { gateErr <- s.SettleJournalBootGate(ctx, cal) }()
	select {
	case e := <-gateErr:
		if e == nil {
			t.Fatal("T26 gate passed while lock held")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("T26 gate did not return on ctx cancel")
	}
	release()
	if err := s.SettleJournalBootGate(context.Background(), cal); err != nil {
		t.Fatalf("T26 gate after release: %v", err)
	}
}
