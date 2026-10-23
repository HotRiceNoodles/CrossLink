package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/model"
)

// ABS-round concurrency counterexamples (review 2026-10-23, round 10).
// The B3/upgrade round raises ABSOLUTELY to a SUM computed in one query
// while the proof-setting pages run as SEPARATE queries — period end is a
// CALENDAR boundary, not a database snapshot. Two deterministic hooks:
//
//   X7: a row committed BETWEEN the SUM and the pages (the request_id=''
//       catch-all admits it at any id) must never be marked-without-count:
//       its proof must not land ahead of its cost, and the next normal
//       round must count it exactly once.
//   X8: a foreign YOUNG row committed BEFORE the round whose owner settles
//       AFTER it: absorbed by the absolute raise + certificate, the late
//       settle must no-op on the mark, release its reservation, and the
//       row must be counted exactly once overall.

func newAbsHarness(t *testing.T) (*BudgetCalibrationService, *redis.Client, *gorm.DB) {
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
	cal := NewBudgetCalibrationService(db, rdb)
	cal.SetJournalGuard(time.Hour)
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })
	return cal, rdb, db
}

// X7: mid-round commit between SUM and pages — no mark-without-count.
func TestCalibrateAbs_X7_MidRoundCommitNotMarkedWithoutCount(t *testing.T) {
	cal, rdb, db := newAbsHarness(t)
	tid := int64(91)
	tm := tid
	pk := PeriodKey("monthly")
	// Pre-round settled state drives the B3 class: budgetset present,
	// watermark absent (proofs "evicted" with it).
	for i := 0; i < 3; i++ {
		if err := db.Create(&model.UsageLog{RequestID: fmt.Sprintf("x7-base-%d", i), TeamID: &tm,
			Cost: 5.0, Currency: "CNY", RouteType: "openai", StatusCode: 200,
			CreatedAt: time.Now().UTC()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	rdb.Set(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk), 15.0, time.Hour)
	rdb.Set(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk), 15.0, time.Hour)
	// Hook AFTER the absolute SUM, BEFORE the first page: a legacy row
	// (request_id='') commits mid-round — outside the SUM, inside the
	// page catch-all under the old predicate.
	calibAbsSumProbe = func() {
		calibAbsSumProbe = nil
		if err := db.Create(&model.UsageLog{RequestID: "", TeamID: &tm,
			Cost: 7.0, Currency: "CNY", RouteType: "openai", StatusCode: 200,
			CreatedAt: time.Now().UTC()}).Error; err != nil {
			t.Errorf("midround insert: %v", err)
		}
	}
	t.Cleanup(func() { calibAbsSumProbe = nil })
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X7 lock")
	}
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X7 round: %v", err)
	}
	rel()
	var mid model.UsageLog
	if err := db.Where("request_id = ''").First(&mid).Error; err != nil {
		t.Fatal(err)
	}
	// The mid-round row must NOT be proofed ahead of its cost: with its
	// cost outside the ABS sum, a proof would mark-without-count forever
	// (no settle path exists for legacy rows).
	if rdb.Exists(context.Background(), fmt.Sprintf("settledmark:team:%d:row-%d", tid, mid.ID)).Val() != 0 ||
		rdb.Exists(context.Background(), fmt.Sprintf("calibcert:team:%d:row-%d", tid, mid.ID)).Val() != 0 {
		t.Fatalf("X7 MARK-WITHOUT-COUNT: mid-round row id=%d proofed while outside the ABS sum", mid.ID)
	}
	b1, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b1 != 15.0 {
		t.Fatalf("X7 round1 budget=%v want 15.0 (mid-round row must not leak into the absolute target)", b1)
	}
	// The next NORMAL round counts it exactly once (no settledmark from
	// round 1 blocking it).
	token2, rel2, _ := cal.acquireCalibrationLock(context.Background())
	if rel2 == nil {
		t.Fatal("X7 lock2")
	}
	defer rel2()
	if _, err := cal.calibrateTarget(context.Background(), token2, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X7 round2: %v", err)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b2 != 22.0 {
		t.Fatalf("X7 round2 budget=%v want 22.0 (base 15 + mid-round 7 counted exactly once)", b2)
	}
}

// X8: foreign young row, settle delayed past the ABS round — absorbed
// once via the raise+certificate; the late settle duplicates, releases
// its reservation; total exactly once.
func TestCalibrateAbs_X8_ForeignYoungDelayedSettle(t *testing.T) {
	cal, rdb, db := newAbsHarness(t)
	tid := int64(92)
	tids := fmt.Sprintf("%d", tid)
	tm := tid
	pk := PeriodKey("monthly")
	foreign := model.UsageLog{RequestID: "x8-foreign", TeamID: &tm, Cost: 4.0,
		Currency: "CNY", RouteType: "openai", StatusCode: 200, CreatedAt: time.Now().UTC()}
	if err := db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	// B3 evidence: settled counter present, watermark gone.
	rdb.Set(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk), 0.0001, time.Hour)
	rdb.Set(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk), 0.0001, time.Hour)
	// The owner reserved e=1 before its (delayed) settle.
	rdb.Set(context.Background(), resvMarkKey("team", tids, "x8-foreign"), fmt.Sprintf("%f|%s", 1.0, pk), time.Hour)
	rdb.IncrByFloat(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk), 1.0)
	rdb.IncrByFloat(context.Background(), fmt.Sprintf("budgetresv:team:%d:%s", tid, pk), 1.0)
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X8 lock")
	}
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X8 round: %v", err)
	}
	rel()
	b, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	s, _ := rdb.Get(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk)).Float64()
	// absorbed cost 4.0 + kept live reservation 1.0 (+ trace baseline)
	if d := b - 5.0; d < -1e-6 || d > 1e-6 {
		t.Fatalf("X8 post-round budget=%v want 5.0 (absorbed 4 + live resv 1; absolute target supersedes the synthetic trace)", b)
	}
	if d := s - 4.0; d < -1e-6 || d > 1e-6 {
		t.Fatalf("X8 post-round settled=%v want 4.0", s)
	}
	if rdb.Exists(context.Background(), settledMarkKey("team", tids, "x8-foreign")).Val() != 1 {
		t.Fatalf("X8 absorbed row must carry the settledmark (late owner settle protection)")
	}
	// The delayed owner settle: duplicate on the mark, reservation released.
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	if code, err := bs.runSettleScript(context.Background(), settleRetryTask{
		scope: "team", targetID: tids, period: "monthly", pk: pk,
		reqID: "x8-foreign", cost: 4.0,
	}); err != nil || code != 2 {
		t.Fatalf("X8 late settle: code=%d err=%v want duplicate(2)", code, err)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	rv, rerr := rdb.Get(context.Background(), fmt.Sprintf("budgetresv:team:%d:%s", tid, pk)).Result()
	rv2 := 0.0
	if rerr == nil {
		fmt.Sscanf(rv, "%f", &rv2)
	}
	if d := b2 - 4.0; d < -1e-6 || d > 1e-6 {
		t.Fatalf("X8 after late settle budget=%v want 4.0 (cost once, reservation released)", b2)
	}
	if rv2 != 0.0 {
		t.Fatalf("X8 resv=%v want 0 (reconciles with live marks)", rv2)
	}
}
