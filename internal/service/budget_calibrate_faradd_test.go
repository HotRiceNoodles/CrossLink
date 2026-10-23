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

// Far-segment additive-convergence counterexamples (review 2026-10-23):
// the far raise used farSum ALONE as the raise target. With an existing
// settled baseline (rows counted while young, since AGED into the far
// domain — production order: old rows have LOWER ids) raise-only cannot
// add: the baseline exceeds the target, nothing moves, yet the far rows
// get settledmarks: their owners/journal then skip = permanently
// uncounted. Also: a duplicate settle (mark present from far
// certification) returned early WITHOUT releasing the owner's reservation
// — estimate/resv residue that never reconciles with live marks.

func newFarHarness(t *testing.T) (*BudgetCalibrationService, *redis.Client, *gorm.DB) {
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
	cal.SetJournalGuard(time.Hour) // far = created older than ~1h10m
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })
	return cal, rdb, db
}

func farSeed(t *testing.T, db *gorm.DB, tid int64, n int, cost float64, age time.Duration, prefix string) {
	t.Helper()
	tm := tid
	rows := make([]model.UsageLog, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, model.UsageLog{RequestID: fmt.Sprintf("%s-%d", prefix, i), TeamID: &tm,
			Cost: cost, Currency: "CNY", RouteType: "openai", StatusCode: 200,
			CreatedAt: time.Now().UTC().Add(-age)})
	}
	if err := db.CreateInBatches(rows, 400).Error; err != nil {
		t.Fatalf("seed %s: %v", prefix, err)
	}
}

// X4 near100+far10 (production id order: OLD rows seeded FIRST, young
// near rows counted first): the far pass must ADD its 10 on top of the
// settled 100 — never mark-and-skip at 100.
func TestCalibrateFar_X4_NearBaselinePlusFarAdds(t *testing.T) {
	cal, rdb, db := newFarHarness(t)
	tid := int64(81)
	pk := PeriodKey("monthly")
	farSeed(t, db, tid, 10, 1.0, 2*time.Hour, "x4-old")  // ids 1-10: FAR domain, total 10
	farSeed(t, db, tid, 10, 10.0, 0, "x4-new")           // ids 11-20: NEAR domain, total 100
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X4 lock")
	}
	defer rel()
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X4 round: %v", err)
	}
	b, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	s, _ := rdb.Get(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk)).Float64()
	if b != 110.0 || s != 110.0 {
		t.Fatalf("X4 MARK-WITHOUT-COUNT: budget=%v settled=%v want 110/110 (near 100 + far 10; far marks must never outrun the counter)", b, s)
	}
	// Idempotence: a second round changes nothing.
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X4 round2: %v", err)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if b2 != 110.0 {
		t.Fatalf("X4 round2 budget=%v want 110 (idempotent)", b2)
	}
}

// X5 cross-page far over an existing baseline: 5001 old rows (two far
// pages) must add exactly their sum on top of the near-settled baseline.
func TestCalibrateFar_X5_CrossPageBaseline(t *testing.T) {
	cal, rdb, db := newFarHarness(t)
	tid := int64(82)
	pk := PeriodKey("monthly")
	farSeed(t, db, tid, 5001, 0.01, 2*time.Hour, "x5-old") // ids low: FAR, 50.01 over 2 pages
	farSeed(t, db, tid, 5, 20.0, 0, "x5-new")              // near baseline 100
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X5 lock")
	}
	defer rel()
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X5 round: %v", err)
	}
	b, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	if d := b - 150.01; d < -1e-6 || d > 1e-6 {
		t.Fatalf("X5 cross-page far: budget=%v want 150.01 (100 near + 50.01 far across two pages, page raises must accumulate)", b)
	}
}

// X6 foreign reserve: one of the far rows carries a LIVE foreign
// reservation (e=2). Far must converge with budget == settled + liveRv
// (112/110/2), and when the owner's settle later hits the far certificate
// (duplicate), it must RELEASE the reservation: 110/110/0, mark gone —
// resv reconciles with live marks, no stranded estimate, no parked overhang.
func TestCalibrateFar_X6_ForeignReserveReconciles(t *testing.T) {
	cal, rdb, db := newFarHarness(t)
	tid := int64(83)
	tids := fmt.Sprintf("%d", tid)
	pk := PeriodKey("monthly")
	farSeed(t, db, tid, 9, 1.0, 2*time.Hour, "x6-old") // ids 1-9 far, total 9
	tm := tid
	if err := db.Create(&model.UsageLog{RequestID: "x6-foreign", TeamID: &tm, Cost: 1.0,
		Currency: "CNY", RouteType: "openai", StatusCode: 200,
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	} // id 10: far, carries the live reservation
	farSeed(t, db, tid, 10, 10.0, 0, "x6-new") // near baseline 100
	// Foreign in-flight request reserved e=2 against the far row.
	rdb.Set(context.Background(), resvMarkKey("team", tids, "x6-foreign"), fmt.Sprintf("%f|%s", 2.0, pk), time.Hour)
	rdb.IncrByFloat(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk), 2.0)
	rdb.IncrByFloat(context.Background(), fmt.Sprintf("budgetresv:team:%d:%s", tid, pk), 2.0)
	rdb.ZAdd(context.Background(), budgetResvExpZSet, redis.Z{
		Score:  float64(time.Now().Add(time.Hour).Unix()),
		Member: resvExpMember("team", tids, "x6-foreign", "monthly", pk, fmt.Sprintf("%f", 2.0)),
	})
	token, rel, _ := cal.acquireCalibrationLock(context.Background())
	if rel == nil {
		t.Fatal("X6 lock")
	}
	defer rel()
	start, end := periodBoundaries("monthly")
	if _, err := cal.calibrateTarget(context.Background(), token, "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("X6 round: %v", err)
	}
	b, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	s, _ := rdb.Get(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk)).Float64()
	rv, _ := rdb.Get(context.Background(), fmt.Sprintf("budgetresv:team:%d:%s", tid, pk)).Float64()
	if s != 110.0 || rv != 2.0 || b != 112.0 {
		t.Fatalf("X6 post-far: budget=%v settled=%v resv=%v want 112/110/2 (settled + liveRv)", b, s, rv)
	}
	// The foreign owner's settle lands on the far certificate: the duplicate
	// path must still release its reservation.
	bs := &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
	if code, err := bs.runSettleScript(context.Background(), settleRetryTask{
		scope: "team", targetID: tids, period: "monthly", pk: pk,
		reqID: "x6-foreign", cost: 1.0,
	}); err != nil || code != 2 {
		t.Fatalf("X6 owner settle: code=%d err=%v want duplicate(2)", code, err)
	}
	b2, _ := rdb.Get(context.Background(), fmt.Sprintf("budget:team:%d:%s", tid, pk)).Float64()
	s2, _ := rdb.Get(context.Background(), fmt.Sprintf("budgetset:team:%d:%s", tid, pk)).Float64()
	rv2v, err2 := rdb.Get(context.Background(), fmt.Sprintf("budgetresv:team:%d:%s", tid, pk)).Result()
	rv2 := 0.0
	if err2 == nil {
		fmt.Sscanf(rv2v, "%f", &rv2)
	}
	mk := rdb.Exists(context.Background(), resvMarkKey("team", tids, "x6-foreign")).Val()
	if b2 != 110.0 || s2 != 110.0 || rv2 != 0.0 || mk != 0 {
		t.Fatalf("X6 after duplicate settle: budget=%v settled=%v resv=%v mark=%d want 110/110/0/0 (reservation released, resv reconciles with live marks)", b2, s2, rv2, mk)
	}
}
