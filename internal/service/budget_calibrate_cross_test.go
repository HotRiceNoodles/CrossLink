package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// Cross-instance counterexamples (review 2026-10-22 round 8): the
// calibration lock only mutual-excludes calibrators, and liveIntentCheck
// only sees THIS process's journal. Another replica's live settle intent
// (in-flight, or killed-unreplayed) is invisible locally, so the NEAR/FAR
// boundary must be derived from the journal guard WINDOW, never from
// MAX(id) at round start: rows created "now" can still carry a live
// intent on another instance, and the FAR script is raise-only WITHOUT
// per-row settledmarks — absorbing such a row and then letting the
// owner's settle land double-counts it.

// X1: another instance's in-flight settle vs an upgrade-round FAR.
// A recent row (settled intent still live elsewhere, no settledmark in
// shared Redis) must NOT be far-absorbed: after the owner's settle lands,
// the counter must equal the cost exactly once.
func TestCalibrateCross_X1_ForeignLiveIntentNotFarAbsorbed(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(41)
	pk := PeriodKey("monthly")
	aTeam := tid
	row := model.UsageLog{RequestID: "x1-live-1", TeamID: &aTeam, Cost: 1.0,
		Currency: "CNY", RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	// Legacy watermark present -> upgrade round (the code path that today
	// sets guardID=MAX(id), pulling the fresh row into FAR).
	if err := rdb.Set(context.Background(), fmt.Sprintf("budgetaudit:team:%d:%s", tid, pk),
		"1.000000000|1690000000", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	// THIS instance's journal is empty — exactly the cross-replica reality
	// liveIntentCheck cannot see through.
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })

	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X1 lock")
	}
	defer rel()
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X1 calibrate: %v", err)
	}
	// Instance A's settle lands now (mark was absent — FAR wrote none).
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	if _, err := bs.runSettleScript(context.Background(), settleRetryTask{
		scope: "team", targetID: fmt.Sprintf("%d", tid), period: "monthly", pk: pk,
		reqID: "x1-live-1", cost: 1.0,
	}); err != nil {
		t.Fatalf("X1 owner settle: %v", err)
	}
	b, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	s, _ := rdb.Get(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk)).Float64()
	if b != 1.0 || s != 1.0 {
		t.Fatalf("X1 DOUBLE-COUNT: budget=%v settled=%v, want exactly 1.0/1.0 (foreign live intent far-absorbed then re-settled)", b, s)
	}
}

// X3: the NEAR/FAR boundary must be the (now-guard) id horizon so rows age
// INTO far monotonically — never MAX(id)-at-round-start, and never frozen
// (today the v3 watermark carries a guardID that no code ever advances,
// so the near segment re-walks the whole period every round).
func TestCalibrateCross_X3_GuardIDIsTimeHorizon(t *testing.T) {
	cal, _, rdb, db := newCalTargetHarness(t)
	tid := int64(43)
	pk := PeriodKey("monthly")
	guard := 2 * time.Hour // tight, test-friendly journal guard
	cal.SetJournalGuard(guard)
	old := time.Now().UTC().Add(-guard - 30*time.Minute)
	aTeam := tid
	olds := make([]model.UsageLog, 0, 2)
	for i := 0; i < 2; i++ {
		olds = append(olds, model.UsageLog{RequestID: fmt.Sprintf("x3-old-%d", i), TeamID: &aTeam,
			Cost: 2.0, Currency: "CNY", RouteType: "openai", StatusCode: 200, CreatedAt: old})
	}
	if err := db.Create(&olds).Error; err != nil {
		t.Fatal(err)
	}
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X3 lock")
	}
	defer rel()
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X3 calibrate-1: %v", err)
	}
	wm, err := rdb.Get(context.Background(), fmt.Sprintf("budgetaudit:team:%d:%s", tid, pk)).Result()
	if err != nil {
		t.Fatalf("X3 watermark: %v", err)
	}
	wantBoundary := olds[1].ID
	parts := strings.Split(wm, "|")
	if len(parts) != 4 || parts[0] != "v3" {
		t.Fatalf("X3 watermark not v3: %q", wm)
	}
	got, gerr := strconv.ParseInt(parts[3], 10, 64)
	if gerr != nil {
		t.Fatalf("X3 watermark guardID unparseable: %q", wm)
	}
	if got != wantBoundary {
		t.Fatalf("X3 guardID=%d want id-horizon %d (wm=%q): rows older than the journal guard must age into FAR so the near segment stays bounded and foreign-live-intent rows can never be far", got, wantBoundary, wm)
	}
	b1, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b1 != 4.0 {
		t.Fatalf("X3 round1 budget=%v want 4.0", b1)
	}
	// X3b: a fresh row arrives (live intent domain) — second round adds it
	// exactly once via NEAR, old rows are NOT re-applied.
	fresh := model.UsageLog{RequestID: "x3-fresh", TeamID: &aTeam, Cost: 1.0,
		Currency: "CNY", RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC()}
	if err := db.Create(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X3 calibrate-2: %v", err)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b2 != 5.0 {
		t.Fatalf("X3 round2 budget=%v want 5.0 (old far idempotent + fresh near once)", b2)
	}
	if rdb.Exists(context.Background(), "settledmark:team:"+fmt.Sprintf("%d", tid)+":x3-fresh").Val() != 1 {
		t.Fatalf("X3 fresh row processed without settledmark — near segment must stay mark-aware")
	}
}

// X2: a killed instance's journal line older than the guard is EXPIRED to
// replay, so its row MUST be far-absorbed (convergence) — and a late boot
// replay attempt of that expired line adds nothing.
func TestCalibrateCross_X2_ExpiredForeignIntentConvergesViaFar(t *testing.T) {
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
	s := &BudgetService{rdb: rdb, markerTTL: time.Minute} // guard ~= 71m
	if err := s.OpenSettleJournal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseSettleJournal)
	cal := NewBudgetCalibrationService(db, rdb)
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })

	tid := int64(42)
	pk := PeriodKey("monthly")
	oldT := time.Now().UTC().Add(-3 * time.Hour)
	aTeam := tid
	row := model.UsageLog{RequestID: "x2-dead-1", TeamID: &aTeam, Cost: 3.0,
		Currency: "CNY", RouteType: "openai", StatusCode: 200, CreatedAt: oldT}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X2 lock")
	}
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X2 calibrate: %v", err)
	}
	rel() // the journal round takes the same lock itself
	b1, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b1 != 3.0 {
		t.Fatalf("X2 far convergence budget=%v want 3.0 (expired-intent row must be absorbed)", b1)
	}
	// Late boot replay of the expired line: the journal round must
	// classify it EXPIRED (calibration owns the heal), not apply it.
	appendJournalLine(t, s.journal.path, "team", fmt.Sprintf("%d", tid), "x2-dead-1", 3.0, oldT.Unix())
	st, rerr := s.SettleJournalRound(context.Background(), cal, 5*time.Second)
	if rerr != nil {
		t.Fatalf("X2 round: %v", rerr)
	}
	if st.Expired != 1 {
		t.Fatalf("X2 expired=%d want 1 (late replay of guard-aged line)", st.Expired)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b2 != 3.0 {
		t.Fatalf("X2 budget after late replay=%v want 3.0 (no double)", b2)
	}
}
