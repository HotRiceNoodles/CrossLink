package service

import (
	"context"
	"fmt"
	"github.com/crosslink/internal/model"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// F1′ deterministic tests: request-idempotent reservation/settlement, the
// mark as settlement ground truth, refund-as-settle-zero, exceeded rollback,
// orphan reaping, and calibration migration + invariant.

func newMarkedRedis(t *testing.T) (*BudgetService, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewBudgetServiceWithMarkerTTL(rdb, time.Hour), rdb, mr
}

func markedKeys(scope, id, pk string) (budget, resv, settled string) {
	return fmt.Sprintf("budget:%s:%s:%s", scope, id, pk),
		budgetResvKey(scope, id, pk), budgetSettledKey(scope, id, pk)
}

func getF(t *testing.T, rdb *redis.Client, key string) float64 {
	t.Helper()
	v, err := rdb.Get(context.Background(), key).Float64()
	if err != nil {
		t.Fatalf("GET %s: %v", key, err)
	}
	return v
}

const mkReq = "req-0001"

// A replayed reserve (response lost, any retry) must not double-increment:
// the mark's SET NX is the guard, and the replay branch reports state.
func TestMarkedReserve_IdempotentReplay(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	_, _, w1, x1 := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, mkReq, 5, 1000)
	if !w1 || x1 {
		t.Fatalf("first reserve: written=%v exceeded=%v", w1, x1)
	}
	spent, _, w2, x2 := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, mkReq, 5, 1000)
	if !w2 || x2 {
		t.Fatalf("replay: written=%v exceeded=%v", w2, x2)
	}
	bk, rk, _ := markedKeys("key", "1", pk)
	if got := getF(t, rdb, bk); got != 5 {
		t.Fatalf("budget after replay = %v, want 5 (no double increment)", got)
	}
	if got := getF(t, rdb, rk); got != 5 {
		t.Fatalf("resv after replay = %v, want 5", got)
	}
	if spent != 5 {
		t.Fatalf("replay spent report = %v, want 5", spent)
	}
}

// Landed reservation settles (cost-e); duplicate settle is a no-op; resv
// released; settled accumulates cost; reaper zset cleaned.
func TestMarkedSettle_LandedExactAndDuplicateNoop(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, mkReq, 5, 1000)
	s.SettleScope(ctx, "key", "1", "monthly", pk, mkReq, 3)
	s.SettleScope(ctx, "key", "1", "monthly", pk, mkReq, 3) // duplicate: no-op

	bk, rk, sk := markedKeys("key", "1", pk)
	if got := getF(t, rdb, bk); got != 3 {
		t.Fatalf("budget = %v, want exact 3 (5 + (3-5))", got)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv = %v, want 0 (released)", got)
	}
	if got := getF(t, rdb, sk); got != 3 {
		t.Fatalf("settled = %v, want 3", got)
	}
	if n, _ := rdb.ZCard(ctx, budgetResvExpZSet).Result(); n != 0 {
		t.Fatalf("reaper zset not cleaned: %d", n)
	}
}

// Un-landed reservation (definitely-not-sent, or already reaped): the settle
// sees no mark and applies FULL cost — never a refund of a phantom.
func TestMarkedSettle_UnlandedFullCost(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	// No reserve for this request id — settle must apply full cost.
	s.SettleScope(ctx, "key", "1", "monthly", pk, "req-noland", 3)
	bk, _, sk := markedKeys("key", "1", pk)
	if got := getF(t, rdb, bk); got != 3 {
		t.Fatalf("budget = %v, want full cost 3", got)
	}
	if got := getF(t, rdb, sk); got != 3 {
		t.Fatalf("settled = %v, want 3", got)
	}
}

// Refund == settle with cost 0 against a held mark (exceeded-rollback path).
func TestMarkedSettle_RefundZero(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, mkReq, 5, 1000)
	s.SettleScope(ctx, "key", "1", "monthly", pk, mkReq, 0)
	bk, rk, sk := markedKeys("key", "1", pk)
	if got := getF(t, rdb, bk); got != 0 {
		t.Fatalf("budget after refund = %v, want 0", got)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv after refund = %v, want 0", got)
	}
	if got := getF(t, rdb, sk); got != 0 {
		t.Fatalf("settled after refund = %v, want 0", got)
	}
}

// Exceeded reservation rolls back atomically INCLUDING the mark: a retry of
// the same request id would then be a fresh reservation, not a replay.
func TestMarkedReserve_ExceededRollsBackMark(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	s.ReportUsage(ctx, "key", "1", "monthly", 9) // seed spent=9
	_, _, w, x := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, mkReq, 5, 10)
	if w || !x {
		t.Fatalf("expected exceeded with written=false, got written=%v exceeded=%v", w, x)
	}
	bk, rk, _ := markedKeys("key", "1", pk)
	if got := getF(t, rdb, bk); got != 9 {
		t.Fatalf("budget after exceeded = %v, want 9 (rolled back)", got)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv after exceeded = %v, want 0", got)
	}
	if rdb.Exists(ctx, resvMarkKey("key", "1", mkReq)).Val() != 0 {
		t.Fatal("mark must be deleted on exceeded rollback")
	}
	// Same request id retry after the abort is a FRESH reservation decision.
	_, _, w2, x2 := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, mkReq, 0.5, 10)
	if !w2 || x2 {
		t.Fatalf("fresh reserve after rollback: written=%v exceeded=%v", w2, x2)
	}
	if got := getF(t, rdb, bk); got != 9.5 {
		t.Fatalf("budget = %v, want 9.5", got)
	}
}

// Orphan reaper: an expired unsettled reservation (crashed request) is
// removed from budget/resv — no permanent over-count; the crashed request's
// ledger cost is restored by the DB-based calibration raise.
func TestReapExpiredReservations_OrphanRemoved(t *testing.T) {
	s, rdb, mr := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, "req-crash", 5, 1000)
	bk, rk, _ := markedKeys("key", "1", pk)

	// Simulate marker expiry (TTL lapsed) with the zset entry due.
	mr.Del(resvMarkKey("key", "1", "req-crash"))
	mr.ZAdd(budgetResvExpZSet, 1, memberForTest("key", "1", "req-crash", "monthly", pk, 5))

	n := s.ReapExpiredReservations(ctx, time.Unix(2, 0))
	if n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	if got := getF(t, rdb, bk); got != 0 {
		t.Fatalf("budget after reap = %v, want 0 (orphan removed)", got)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv after reap = %v, want 0", got)
	}
	if c, _ := rdb.ZCard(ctx, budgetResvExpZSet).Result(); c != 0 {
		t.Fatalf("zset not cleaned: %d", c)
	}
	// Live marks are re-scheduled, not reaped.
	s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, "req-live", 2, 1000)
	mr.ZAdd(budgetResvExpZSet, 1, memberForTest("key", "1", "req-live", "monthly", pk, 2))
	n2 := s.ReapExpiredReservations(ctx, time.Unix(2, 0))
	if n2 != 0 {
		t.Fatalf("live reservation reaped: %d", n2)
	}
	if got := getF(t, rdb, bk); got != 2 {
		t.Fatalf("live reservation disturbed: budget = %v, want 2", got)
	}
}

func memberForTest(scope, id, req, period, pk string, e float64) string {
	return resvExpMember(scope, id, req, period, pk, fmt.Sprintf("%.9f", e))
}

// End-to-end through ReserveForRequest with a request id: reserve on three
// scopes, settle all three with actual cost — EXACT at every layer, and the
// invariant budget == settled + resv holds.
func TestMarkedReserveForRequest_ThreeScopesExact(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	scopes := []BudgetScope{
		{Scope: "key", ID: "990103", Period: "monthly", Limit: 1000},
		{Scope: "team", ID: "990102", Period: "monthly", Limit: 1000},
		{Scope: "org", ID: "990101", Period: "monthly", Limit: 1000},
	}
	res, exceeded := s.ReserveForRequest(ctx, scopes, 5, "req-e2e")
	if exceeded != "" || len(res.Reserved) != 3 {
		t.Fatalf("reserve: exceeded=%q reserved=%d", exceeded, len(res.Reserved))
	}
	for _, sc := range scopes {
		s.SettleScope(ctx, sc.Scope, sc.ID, sc.Period, res.PeriodKeys[sc.Scope], "req-e2e", 3)
		bk, rk, sk := markedKeys(sc.Scope, sc.ID, res.PeriodKeys[sc.Scope])
		if got := getF(t, rdb, bk); got != 3 {
			t.Fatalf("%s budget = %v, want 3", sc.Scope, got)
		}
		if got := getF(t, rdb, rk); got != 0 {
			t.Fatalf("%s resv = %v, want 0", sc.Scope, got)
		}
		if got := getF(t, rdb, sk); got != 3 {
			t.Fatalf("%s settled = %v, want 3", sc.Scope, got)
		}
	}
}

// Migration + invariant: a pre-F1′ counter (budget only) is migrated by the
// calibration pass (settled=SUM(DB), resv=overhang); the second pass checks
// the invariant without touching values.
func TestCalibration_MigratesThenChecksInvariant(t *testing.T) {
	a, _, mr, db := newCalibrationFixture(t)
	rdb := a.rdb
	ctx := context.Background()

	// The calibration write requires lock ownership.
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)

	// Pre-F1' state: a team counter raised by old-style writes only.
	var tm model.Team
	if err := db.First(&tm).Error; err != nil {
		t.Fatalf("fixture team: %v", err)
	}
	pk := PeriodKey("monthly")
	bk, rk, sk := markedKeys("team", fmt.Sprintf("%d", tm.ID), pk)
	rdb.Set(ctx, bk, 12.5, time.Hour)

	if err := a.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 100); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// raise-only write: budget 12.5 stays (above snapshot 10).
	if got := getF(t, rdb, bk); got != 12.5 {
		t.Fatalf("budget = %v, want unchanged 12.5", got)
	}
	// Migration: settled = SUM(DB) = 10, resv = overhang 2.5.
	if got := getF(t, rdb, sk); got != 10 {
		t.Fatalf("migrated settled = %v, want 10", got)
	}
	if got := getF(t, rdb, rk); got != 2.5 {
		t.Fatalf("migrated resv = %v, want 2.5", got)
	}

	// Second round (same owner): invariant holds (12.5 == 10 + 2.5) — values
	// untouched, migration not repeated (SetNX already consumed).
	if err := a.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 100); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := getF(t, rdb, sk); got != 10 {
		t.Fatalf("settled after second round = %v, want 10 (no re-migration)", got)
	}
	_ = mr
}
