package service

// v3-review regressions for calibrateTarget's crash-atomic certification:
//   C1 raise+mark are ONE script invocation — a rejected (stale-token)
//       apply leaves BOTH untouched (covered also by the ported lock
//       test); here the accepted side is pinned per-page.
//   C2 rows INSERTED mid-round: a later-id row is picked up by the next
//       page (counted+marked together); an earlier-id row behind the
//       cursor is neither counted nor marked (owned by the settle path or
//       the journal — never silently absorbed).
//   C3 >50000 rows: keyset paging processes every row (no Limit loss).
//   C4 a Redis failure mid-round aborts the target with the budgetaudit
//       watermark NOT advanced; the next round re-pages from the old
//       watermark and completes (idempotent raise+SET).

import (
	"strings"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

func newCalTargetHarness(t *testing.T) (*BudgetCalibrationService, *miniredis.Miniredis, *redis.Client, *gorm.DB) {
	t.Helper()
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
	return NewBudgetCalibrationService(db, rdb), mr, rdb, db
}

func seedRows(t *testing.T, db *gorm.DB, teamID int64, n int, cost float64) {
	t.Helper()
	tid := teamID
	rows := make([]model.UsageLog, 0, n)
	now := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < n; i++ {
		rows = append(rows, model.UsageLog{
			RequestID: fmt.Sprintf("c-%06d", i), TeamID: &tid, Cost: cost, Currency: "CNY",
			RouteType: "openai", StatusCode: 200, CreatedAt: now,
		})
	}
	if err := db.CreateInBatches(rows, 500).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestCalibrateTarget_C1_AtomicBothOrNeither(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(31)
	seedRows(t, db, tid, 3, 1.0)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	token1, rel1, _ := cal.acquireCalibrationLock(context.Background())
	if rel1 == nil {
		t.Fatal("C1 lock")
	}
	defer rel1()
	if _, err := cal.calibrateTarget(context.Background(), token1, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("C1: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:31:"+pk).Float64()
	if b != 3.0 {
		t.Fatalf("C1 budget=%v want 3.0", b)
	}
	for i := 0; i < 3; i++ {
		if rdb.Exists(context.Background(), settledMarkKey("team", "31", fmt.Sprintf("c-%06d", i))).Val() != 1 {
			t.Fatalf("C1 row c-%06d not certified atomically with the raise", i)
		}
	}
}

// C2: mid-round inserts — later-id row joins a later page (counted+marked);
// earlier-id row behind the cursor is neither counted nor marked.
func TestCalibrateTarget_C2_MidRoundInserts(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(32)
	seedRows(t, db, tid, 5001, 1.0) // page 0 fills (5000), page 1 holds the tail
	// Pre-place a HIGH id so the mid-round insert's new id lands above the
	// page-0 cursor deterministically.
	db.Exec("UPDATE usage_logs SET id = 1000 WHERE id = (SELECT MIN(id) FROM usage_logs)")
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	calibPageProbe = func(page int) {
		if page == 0 {
			tm := tid
			// Later-id row: must be counted+marked by page 2.
			db.Create(&model.UsageLog{RequestID: "late-high", TeamID: &tm, Cost: 7.0, Currency: "CNY",
				RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-30 * time.Second)})
			// Earlier-id row (behind the page-1 cursor id>1000): neither
			// counted nor marked this round — the settle/journal path owns
			// it. Created with an EXPLICIT id (no UPDATE race).
			db.Create(&model.UsageLog{ID: 6, RequestID: "late-low", TeamID: &tm, Cost: 13.0, Currency: "CNY",
				RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-30 * time.Second)})
			// Drop the probe so subsequent pages proceed untouched.
			calibPageProbe = nil
		}
	}
	t.Cleanup(func() { calibPageProbe = nil })
	token2, rel2, _ := cal.acquireCalibrationLock(context.Background())
	if rel2 == nil {
		t.Fatal("C2 lock")
	}
	defer rel2()
	spent, err := cal.calibrateTarget(context.Background(), token2, "team", tid, pk, "monthly", start, end)
	if err != nil {
		t.Fatalf("C2: %v", err)
	}
	// 5001 seeded + late-high(7) = 5008; late-low(13) must be EXCLUDED.
	if spent != 5008.0 {
		t.Fatalf("C2 spent=%v want 5008.0 (late-low behind cursor excluded)", spent)
	}
	if rdb.Exists(context.Background(), settledMarkKey("team", "32", "late-high")).Val() != 1 {
		t.Fatal("C2 late-high row not certified (must join a later page atomically)")
	}
	if rdb.Exists(context.Background(), settledMarkKey("team", "32", "late-low")).Val() != 0 {
		t.Fatal("C2 late-low row was marked WITHOUT being counted — journal would skip an un-billed row")
	}
	b, _ := rdb.Get(context.Background(), "budget:team:32:"+pk).Float64()
	if b != 5008.0 {
		t.Fatalf("C2 budget=%v want 5008.0", b)
	}
}

// C3: 50,001 rows — every row paged, counted and certified exactly once.
func TestCalibrateTarget_C3_PastFiftyThousand(t *testing.T) {
	if testing.Short() {
		t.Skip("55k-row sqlite seed is slow in -short")
	}
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(33)
	seedRows(t, db, tid, 50001, 0.001)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	token3, rel3, _ := cal.acquireCalibrationLock(context.Background())
	if rel3 == nil {
		t.Fatal("C3 lock")
	}
	defer rel3()
	spent, err := cal.calibrateTarget(context.Background(), token3, "team", tid, pk, "monthly", start, end)
	if err != nil {
		t.Fatalf("C3: %v", err)
	}
	if d := spent - 50.001; d < -1e-6 || d > 1e-6 {
		t.Fatalf("C3 spent=%v want 50.001 (50001 x 0.001, no page lost)", spent)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:33:"+pk).Float64()
	if d := b - 50.001; d < -1e-6 || d > 1e-6 {
		t.Fatalf("C3 budget=%v want 50.001", b)
	}
	var marked int64
	for _, k := range rdb.Keys(context.Background(), "settledmark:team:33:c-*").Val() {
		_ = k
		marked++
	}
	if marked != 50001 {
		t.Fatalf("C3 certified=%d want 50001", marked)
	}
}

// C4: Redis failure mid-round aborts the target WITHOUT advancing the
// watermark; the next round re-pages from the old watermark and completes.
func TestCalibrateTarget_C4_RedisFailureNoWatermarkAdvance(t *testing.T) {
	cal, mr, rdb, db := newCalTargetHarness(t)
	tid := int64(34)
	seedRows(t, db, tid, 4, 1.0)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	probeFired := false
	calibPageProbe = func(page int) {
		if !probeFired {
			probeFired = true
			mr.SetError("redis-down-mid-round")
		}
	}
	t.Cleanup(func() { calibPageProbe = nil })
	token4, rel4, _ := cal.acquireCalibrationLock(context.Background())
	if rel4 == nil {
		t.Fatal("C4 lock")
	}
	_, err := cal.calibrateTarget(context.Background(), token4, "team", tid, pk, "monthly", start, end)
	mr.SetError("")
	rel4()
	// Simulate the failed holder's lease expiring so the retry can lock.
	rdb.Del(context.Background(), calibrationLockKey)
	if err == nil {
		t.Fatal("C4 expected mid-round failure")
	}
	if rdb.Exists(context.Background(), "budgetaudit:team:34:"+pk).Val() == 1 {
		t.Fatal("C4 watermark ADVANCED past a failed round — the v3-review bug")
	}
	mr.SetError("")
	calibPageProbe = nil
	token4b, rel4b, _ := cal.acquireCalibrationLock(context.Background())
	if rel4b == nil {
		t.Fatal("C4b lock")
	}
	defer rel4b()
	spent, err := cal.calibrateTarget(context.Background(), token4b, "team", tid, pk, "monthly", start, end)
	if err != nil {
		t.Fatalf("C4 retry: %v", err)
	}
	if spent != 4.0 {
		t.Fatalf("C4 retry spent=%v want 4.0", spent)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:34:"+pk).Float64()
	if b != 4.0 {
		t.Fatalf("C4 retry budget=%v want 4.0 (idempotent raise, not doubled)", b)
	}
}

// ---- v4 review: deterministic under-count counterexamples (RED first) ----

// R1 (baseline bug): counter already at 100 from settled history; a later
// window adds 10 rows whose settles were LOST (no marks). The round must
// bring the counter to 110 and mark them — the old incremental
// raise-to-page-sum left the counter at 100 while marking the rows
// (journal then skips them forever: permanent under-count).
func TestCalibrateTarget_R1_BaselinePlusLostSettles(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(41)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	// Settled history: one 100.0 row, settled normally (mark present).
	tm := tid
	db.Create(&model.UsageLog{RequestID: "hist-1", TeamID: &tm, Cost: 100.0, Currency: "CNY",
		RouteType: "openai", StatusCode: 200, CreatedAt: start.Add(time.Minute)})
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	_ = tok
	if rel == nil {
		t.Fatal("lock")
	}
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	bs.SettleScope(context.Background(), "team", "41", "monthly", pk, "hist-1", 100.0)
	// Round 1 completes and FINALIZES the watermark — the under-count bug
	// is a SECOND-round property (incremental window vs absolute baseline).
	if _, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R1 round1: %v", err)
	}
	cal.finalizeTarget(context.Background(), "team", tid, pk, "monthly", 100.0, 1000)
	rel()
	// Lost settles: 10 rows, cost 1.0 each, NO marks.
	for i := 0; i < 10; i++ {
		db.Create(&model.UsageLog{RequestID: fmt.Sprintf("lost-%02d", i), TeamID: &tm, Cost: 1.0, Currency: "CNY",
			RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	}
	tok2, rel2, _ := cal.acquireCalibrationLock(context.Background())
	if rel2 == nil {
		t.Fatal("lock2")
	}
	defer rel2()
	if _, err := cal.calibrateTarget(context.Background(), tok2, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R1: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:41:"+pk).Float64()
	if b != 110.0 {
		t.Fatalf("R1 budget=%v want 110.0 (baseline 100 + 10 lost rows; marking without adding is the under-count bug)", b)
	}
	for i := 0; i < 10; i++ {
		if rdb.Exists(context.Background(), settledMarkKey("team", "41", fmt.Sprintf("lost-%02d", i))).Val() != 1 {
			t.Fatalf("R1 lost-%02d not marked", i)
		}
	}
}

// R2 (cross-page partial sums): baseline 100, then 6001 lost rows (two+
// pages whose early partial sums sit below the baseline) — every row must
// land exactly once (100+60.01), no page may "mark without adding".
func TestCalibrateTarget_R2_CrossPagePartialSumsBelowBaseline(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(42)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	tm := tid
	db.Create(&model.UsageLog{RequestID: "hist-1", TeamID: &tm, Cost: 100.0, Currency: "CNY",
		RouteType: "openai", StatusCode: 200, CreatedAt: start.Add(time.Minute)})
	_, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	bs.SettleScope(context.Background(), "team", "42", "monthly", pk, "hist-1", 100.0)
	rel()
	seedRows(t, db, tid, 6001, 0.01) // 6001 rows x 0.01, no marks
	tok2, rel2, _ := cal.acquireCalibrationLock(context.Background())
	if rel2 == nil {
		t.Fatal("lock2")
	}
	defer rel2()
	if _, err := cal.calibrateTarget(context.Background(), tok2, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R2: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:42:"+pk).Float64()
	if d := b - 160.01; d < -1e-6 || d > 1e-6 {
		t.Fatalf("R2 budget=%v want 160.01 (100 + 6001x0.01, each row exactly once across pages)", b)
	}
}

// R3 (late-arriving old-created_at row / WAL-replay shape): after a full
// round, a row with an OLD created_at but a NEW id appears (WAL replay
// commits late). The next round must pick it up by the id cursor — a
// time-based watermark would skip it forever.
func TestCalibrateTarget_R3_LateRowOldCreatedAtNewID(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(43)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	tm := tid
	seedRows(t, db, tid, 3, 1.0)
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	if spent1, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R3 round1: %v", err)
	} else {
		cal.finalizeTarget(context.Background(), "team", tid, pk, "monthly", spent1, 1000)
	}
	rel()
	// WAL-replay shape: OLD created_at (round-1 window), NEW id.
	late := model.UsageLog{RequestID: "late-wal", TeamID: &tm, Cost: 5.0, Currency: "CNY",
		RouteType: "openai", StatusCode: 200, CreatedAt: start.Add(10 * time.Minute)}
	if err := db.Create(&late).Error; err != nil {
		t.Fatal(err)
	}
	tok2, rel2, _ := cal.acquireCalibrationLock(context.Background())
	if rel2 == nil {
		t.Fatal("lock2")
	}
	defer rel2()
	if _, err := cal.calibrateTarget(context.Background(), tok2, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R3 round2: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:43:"+pk).Float64()
	if b != 8.0 {
		t.Fatalf("R3 budget=%v want 8.0 (late WAL row must be picked up by the id cursor)", b)
	}
	if rdb.Exists(context.Background(), settledMarkKey("team", "43", "late-wal")).Val() != 1 {
		t.Fatal("R3 late row not marked")
	}
}

// ---- v5 review regressions ----

// R4 (review ②): a row arriving AFTER the near pages but BEFORE the far
// SUM must NOT be absorbed by the SUM (its journal intent is still live)
// — the journal replay then applies it exactly once.
func TestCalibrateTarget_R4_LateRowNotAbsorbedByFarSum(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(44)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	tm := tid
	seedRows(t, db, tid, 3, 1.0)
	probeOnce := true
	calibPageProbe = func(page int) {
		if probeOnce && page == 0 {
			probeOnce = false
			db.Create(&model.UsageLog{ID: 999999, RequestID: "r4-late", TeamID: &tm, Cost: 9.0, Currency: "CNY",
				RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-time.Minute)})
			calibPageProbe = nil
		}
	}
	t.Cleanup(func() { calibPageProbe = nil })
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	spent, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end)
	rel()
	if err != nil {
		t.Fatalf("R4: %v", err)
	}
	// Round 1 saw pages before the late row existed in any page (id above
	// the page cursor only if a later page ran — it did not: <5000 rows),
	// and the far SUM must exclude it (guard-window row).
	if spent != 3.0 {
		t.Fatalf("R4 round1 spent=%v want 3.0 (late row NOT absorbed by far SUM)", spent)
	}
	if rdb.Exists(context.Background(), settledMarkKey("team", "44", "r4-late")).Val() != 0 {
		t.Fatal("R4 late row was marked without being near-paged")
	}
	// Journal replay applies it exactly once.
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	if err := bs.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.CloseSettleJournal)
	appendJournalLine(t, bs.journal.path, "team", "44", "r4-late", 9.0, time.Now().Unix())
	if _, rerr := bs.SettleJournalRound(context.Background(), cal, 5*time.Second); rerr != nil {
		t.Fatalf("R4 journal round: %v", rerr)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:44:"+pk).Float64()
	if b != 12.0 {
		t.Fatalf("R4 budget=%v want 12.0 (3 + journal-applied 9, exactly once)", b)
	}
}

// R5 (review ③): a legacy "spent|unix-seconds" watermark must NOT be read
// as an id cursor (its magnitude would skip every newer row) — it is
// discarded and the round re-scans idempotently.
func TestCalibrateTarget_R5_LegacyWatermarkUpgrade(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(45)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	seedRows(t, db, tid, 4, 1.0)
	// Legacy timestamp-shaped watermark: 1777000000 as "cursor".
	rdb.Set(context.Background(), "budgetaudit:team:45:"+pk, "4.000000000|1777000000", time.Hour)
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	defer rel()
	_, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end)
	if err != nil {
		t.Fatalf("R5: %v", err)
	}
	// Upgrade round: all rows are FAR — the budget counter converges to
	// their sum via raise-only (budgetset convergence is finalize's step).
	fb, _ := rdb.Get(context.Background(), "budget:team:45:"+pk).Float64()
	if fb != 4.0 {
		t.Fatalf("R5 budget=%v want 4.0 (legacy watermark discarded, far convergence re-scanned)", fb)
	}
	// New watermark carries the v2 prefix with the real id cursor.
	wm, _ := rdb.Get(context.Background(), "budgetaudit:team:45:"+pk).Result()
	if !strings.HasPrefix(wm, "v3|") {
		t.Fatalf("R5 watermark=%q want v3| prefix", wm)
	}
}

// R6 (review ①): a near-segment row whose usage landed while the ORIGINAL
// settle is still in flight (reserve mark present) must be driven through
// the state-aware script: budget gains (cost-e), the reservation is
// released, and the later original settle no-ops on the mark.
func TestCalibrateTarget_R6_InFlightSettleEstimateReleased(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(46)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	tm := tid
	// Live reservation e=2 for req r6-1 (settle not yet run).
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	bs.ReserveScopeMarked(context.Background(), "team", "46", "monthly", pk, "r6-1", 2.0, 1000)
	// Usage row for the same request: cost 5.
	db.Create(&model.UsageLog{RequestID: "r6-1", TeamID: &tm, Cost: 5.0, Currency: "CNY",
		RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	defer rel()
	if _, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R6: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:46:"+pk).Float64()
	if b != 5.0 {
		t.Fatalf("R6 budget=%v want 5.0 (reserve 2 + (cost-e)=3; estimate released exactly)", b)
	}
	rv, _ := rdb.Get(context.Background(), "budgetresv:team:46:"+pk).Float64()
	if rv != 0 {
		t.Fatalf("R6 resv=%v want 0 (reservation released by the state-aware apply)", rv)
	}
	// The original settle arriving later is a mark no-op (no double).
	bs.SettleScope(context.Background(), "team", "46", "monthly", pk, "r6-1", 5.0)
	b2, _ := rdb.Get(context.Background(), "budget:team:46:"+pk).Float64()
	if b2 != 5.0 {
		t.Fatalf("R6 after original settle budget=%v want 5.0 (mark guard held)", b2)
	}
}

// R7 (review ④): far-segment rows (older than the guard window) whose
// marks expired converge via the SUM exactly once — and a journal line for
// them (past its own guard) is EXPIRED, never re-applied: no double.
func TestCalibrateTarget_R7_FarSegmentExactlyOnce(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(47)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	tm := tid
	// FAR row: created 72h ago (outside any default guard).
	db.Create(&model.UsageLog{RequestID: "far-1", TeamID: &tm, Cost: 6.0, Currency: "CNY",
		RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC().Add(-72 * time.Hour)})
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	if _, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R7 round1: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:47:"+pk).Float64()
	if b != 6.0 {
		t.Fatalf("R7 round1 budget=%v want 6.0 (far SUM convergence)", b)
	}
	// Second round: idempotent.
	if _, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("R7 round2: %v", err)
	}
	b2, _ := rdb.Get(context.Background(), "budget:team:47:"+pk).Float64()
	if b2 != 6.0 {
		t.Fatalf("R7 round2 budget=%v want 6.0 (raise-only idempotent)", b2)
	}
	rel() // release BEFORE the journal round needs the same lock
	// An EXPIRED journal line for the far row must not re-apply.
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	if err := bs.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.CloseSettleJournal)
	appendJournalLine(t, bs.journal.path, "team", "47", "far-1", 6.0, time.Now().Add(-72*time.Hour).Unix())
	st, rerr := bs.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if rerr != nil {
		t.Fatalf("R7 journal: %v", rerr)
	}
	if st.Expired != 1 || st.Recovered != 0 {
		t.Fatalf("R7 journal stats=%+v want expired=1 recovered=0", st)
	}
	b3, _ := rdb.Get(context.Background(), "budget:team:47:"+pk).Float64()
	if b3 != 6.0 {
		t.Fatalf("R7 after journal budget=%v want 6.0 (expired line never re-applied)", b3)
	}
}

// ---- v6 review regressions (durable idempotency boundary) ----

// B1: an upgrade round must NOT bulk-absorb rows while the journal still
// holds LIVE intents — the FAR segment defers and the row is settled
// exactly once by its own path.
func TestCalibrateTarget_B1_UpgradeDefersFarWhenJournalLive(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(51)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	seedRows(t, db, tid, 3, 1.0)
	// Legacy watermark present -> upgrade round would put everything FAR.
	rdb.Set(context.Background(), "budgetaudit:team:51:"+pk, "3.0|1777000000", time.Hour)
	// A journal with ONE live intent whose row is NOT in the DB yet.
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	if err := bs.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bs.CloseSettleJournal)
	if err := bs.journal.append(settleRetryTask{scope: "team", targetID: "51", period: "monthly", pk: pk, reqID: "live-1", cost: 4.0}); err != nil {
		t.Fatal(err)
	}
	cal.SetLiveIntentCheck(bs.JournalLiveIntents)
	t.Cleanup(func() { cal.SetLiveIntentCheck(nil) })
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	defer rel()
	if _, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("B1: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:51:"+pk).Float64()
	if b != 0 {
		t.Fatalf("B1 budget=%v want 0 (FAR deferred while journal live — no bulk absorb)", b)
	}
	// The live intent settles exactly once via its own path.
	bs.SettleScope(context.Background(), "team", "51", "monthly", pk, "live-1", 4.0)
	b2, _ := rdb.Get(context.Background(), "budget:team:51:"+pk).Float64()
	if b2 != 4.0 {
		t.Fatalf("B2 after settle budget=%v want 4.0", b2)
	}
}

// B2: a cert-write failure aborts the target WITHOUT advancing the
// watermark; the retry lands on the still-live settledmark (no-op) and
// writes the cert — no double application at any point.
func TestCalibrateTarget_B2_CertWriteFailureRetriesSafe(t *testing.T) {
	cal, mr, rdb, db := newCalTargetHarness(t)
	tid := int64(52)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	seedRows(t, db, tid, 2, 1.0)
	failCert := true
	calibPageProbe = func(page int) {
		if failCert {
			failCert = false
			// Break Redis AFTER the page's settle applies succeed but the
			// per-row cert writes fail -> target must abort.
			mr.SetError("cert-write-outage")
		}
	}
	t.Cleanup(func() { calibPageProbe = nil; mr.SetError("") })
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	_, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end)
	mr.SetError("")
	rel()
	if err == nil {
		t.Fatal("B2 expected cert-write failure to abort the round")
	}
	if rdb.Exists(context.Background(), "budgetaudit:team:52:"+pk).Val() == 1 {
		t.Fatal("B2 watermark advanced past a failed round")
	}
	// Retry once Redis is back: settledmark (fresh, inside its guard)
	// shields the rows; certs land; counters exact.
	calibPageProbe = nil
	tok2, rel2, _ := cal.acquireCalibrationLock(context.Background())
	if rel2 == nil {
		t.Fatal("lock2")
	}
	defer rel2()
	if _, err := cal.calibrateTarget(context.Background(), tok2, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("B2 retry: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:52:"+pk).Float64()
	if b != 2.0 {
		t.Fatalf("B2 budget=%v want 2.0 (mark shielded the retry; cert landed)", b)
	}
}

// B3: a VANISHED watermark over an existing counter is an upgrade round
// (all pre-existing rows FAR) — the near regime must never retro-claim
// rows whose settledmarks may have expired.
func TestCalibrateTarget_B3_VanishedWatermarkIsUpgrade(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(53)
	start, end := periodBoundaries("monthly")
	pk := PeriodKey("monthly")
	tm := tid
	seedRows(t, db, tid, 3, 5.0)
	// Counter exists (settled long ago), NO watermark key at all, and the
	// rows' settledmarks are long gone — the worst idempotency shape.
	rdb.Set(context.Background(), "budget:team:53:"+pk, "15.0", time.Hour)
	rdb.Set(context.Background(), "budgetset:team:53:"+pk, "15.0", time.Hour)
	tok, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("lock")
	}
	defer rel()
	spent, err := cal.calibrateTarget(context.Background(), tok, "team", tid, pk, "monthly", start, end)
	if err != nil {
		t.Fatalf("B3: %v", err)
	}
	b, _ := rdb.Get(context.Background(), "budget:team:53:"+pk).Float64()
	if b != 15.0 {
		t.Fatalf("B3 budget=%v want 15.0 (vanished watermark -> upgrade/FAR convergence, never per-row re-add)", b)
	}
	_ = spent
	_ = tm
}
