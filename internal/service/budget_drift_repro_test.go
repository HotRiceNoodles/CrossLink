package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Deterministic drift reproduction suite (supervisor directive 2026-10-21):
// r35-4h ended with budget==settled==DB and resv=-0.736 (negative); r44-4h
// ended the same three-way exact but resv=+0.005984/+0.020944 (positive,
// exact multiples of the per-request estimate, no live marks, empty ZSET).
// These tests interleave reserve / settle / reap / calibration on miniredis
// with a per-step snapshot, and assert the quiescent contract:
//   no pending orders  =>  resv == 0 AND budget == settled == ledger truth
//   invariant          =>  budget == settled + resv  (every snapshot)
// They are expected to FAIL on the unfixed code — that failure IS the
// deterministic regression the review demanded (original r44 FAILs kept).

type driftSnap struct {
	step        string
	b, s, r     float64
	marks, zset int
	drift       float64
}

func snapDrift(t *testing.T, rdb *redis.Client, step, scope, id, pk string) driftSnap {
	t.Helper()
	bk, rk, sk := markedKeys(scope, id, pk)
	b, _ := rdb.Get(context.Background(), bk).Float64()
	s, _ := rdb.Get(context.Background(), sk).Float64()
	r, _ := rdb.Get(context.Background(), rk).Float64()
	marks := 0
	iter := rdb.Scan(context.Background(), 0, "resvmark:*", 100).Iterator()
	for iter.Next(context.Background()) {
		marks++
	}
	z := int(rdb.ZCard(context.Background(), budgetResvExpZSet).Val())
	return driftSnap{step: step, b: b, s: s, r: r, marks: marks, zset: z, drift: b - s - r}
}

func (d driftSnap) String() string {
	return fmt.Sprintf("%-30s b=%.6f s=%.6f r=%.6f drift=%+.6f marks=%d zset=%d", d.step, d.b, d.s, d.r, d.drift, d.marks, d.zset)
}

func logSnaps(t *testing.T, snaps ...driftSnap) {
	for _, s := range snaps {
		t.Log(s.String())
	}
}

func quiescenceChecks(t *testing.T, s driftSnap, wantSpent float64, label string) {
	t.Helper()
	require.Equal(t, 0, s.marks, "%s: no pending orders expected: %s", label, s)
	require.InDelta(t, 0, s.r, 1e-9, "%s: resv must be 0 at quiescence: %s", label, s)
	require.InDelta(t, wantSpent, s.s, 1e-9, "%s: settled==ledger: %s", label, s)
	require.InDelta(t, wantSpent, s.b, 1e-9, "%s: budget==ledger: %s", label, s)
}

// T1 — r35-negative shape: first-sight calibration runs while a crash
// cohort's ledger rows exist (settles lost) AND a live reservation is held.
// The migration r := budget - spent clobbers the live reservation; the
// later settle then drives resv negative.
func TestDriftRepro_T1_MigrationDuringFaultLag(t *testing.T) {
	_, _, mr, dbF := newCalibrationFixture(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cs := NewBudgetCalibrationService(dbF, rdb)
	bs := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Hour)
	ctx := context.Background()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	var tm struct{ ID int64 }
	require.NoError(t, dbF.Table("teams").Select("id").First(&tm).Error)
	id := fmt.Sprintf("%d", tm.ID)
	pk := PeriodKey("monthly")

	// 1. crash cohort: ledger row cost=10 whose settle was lost (counters
	//    never saw it) — injected deterministically via the round's spent.
	// 2. live request reserves e=3.
	_, _, w, x := bs.ReserveScopeMarked(ctx, "team", id, "monthly", pk, "req-t1", 3.0, 1000)
	require.True(t, w)
	require.False(t, x)
	s0 := snapDrift(t, rdb, "after reserve e=3", "team", id, pk)
	logSnaps(t, s0)
	require.InDelta(t, 0, s0.drift, 1e-9, "reserve must preserve invariant")

	// 3. first-sight calibration round sees ledger spent=10 (crash row).
	require.NoError(t, cs.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 1000))
	s1 := snapDrift(t, rdb, "after round spent=10", "team", id, pk)
	logSnaps(t, s1)

	// 4. live request settles normally (ledger truth is now 13).
	bs.SettleScope(ctx, "team", id, "monthly", pk, "req-t1", 3.0)
	s2 := snapDrift(t, rdb, "after live settle c=3", "team", id, pk)
	logSnaps(t, s2)

	// Round 2 with the full ledger truth 13 (settles+reconcile converged).
	require.NoError(t, cs.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", 13.0, 1000))
	s3 := snapDrift(t, rdb, "after round spent=13", "team", id, pk)
	logSnaps(t, s3)

	quiescenceChecks(t, s3, 13, "T1")
}

// T2 — r44-positive shape hunt: lost settle + settled-raise while the
// reservation is still live, then mark-TTL reap.
func TestDriftRepro_T2_LostSettleRaiseThenReap(t *testing.T) {
	_, _, mr, dbF := newCalibrationFixture(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cs := NewBudgetCalibrationService(dbF, rdb)
	bs := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Hour)
	ctx := context.Background()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	var tm struct{ ID int64 }
	require.NoError(t, dbF.Table("teams").Select("id").First(&tm).Error)
	id := fmt.Sprintf("%d", tm.ID)
	pk := PeriodKey("monthly")

	// Round 1: quiet first-sight at spent=10 (all settles applied).
	require.NoError(t, cs.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 1000))
	s0 := snapDrift(t, rdb, "round1 spent=10", "team", id, pk)
	logSnaps(t, s0)

	// Request reserves e=2, completes with cost=2 (ledger 12), settle LOST.
	_, _, w, _ := bs.ReserveScopeMarked(ctx, "team", id, "monthly", pk, "req-t2", 2.0, 1000)
	require.True(t, w)
	s1 := snapDrift(t, rdb, "reserve e=2 (settle lost)", "team", id, pk)
	logSnaps(t, s1)

	// Round 2 with spent=12: budget-raise and settled-raise run against a
	// namespace that still holds a live reservation.
	require.NoError(t, cs.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", 12.0, 1000))
	s2 := snapDrift(t, rdb, "round2 spent=12", "team", id, pk)
	logSnaps(t, s2)

	// Mark TTL expires (the marker outlives any legitimate request); the
	// reaper then reclaims the orphan reservation. FastForward expires the
	// mark key on the virtual clock; the passed `now` must also cover the
	// zset score horizon (reserve-time + markerTTL).
	mr.FastForward(2*time.Hour + time.Minute)
	reaped := bs.ReapExpiredReservations(ctx, time.Now().Add(3*time.Hour))
	require.Equal(t, 1, reaped)
	s3 := snapDrift(t, rdb, "after reap", "team", id, pk)
	logSnaps(t, s3)
	quiescenceChecks(t, s3, 12, "T2")
}

// T3 — cross-period settle: reserve lands in pk_old (period crossing at
// admission), settle carries pk_new (empty PeriodKeys entry -> FINDING-B
// fallback). Quiescent contract must hold in BOTH periods.
func TestDriftRepro_T3_CrossPeriodSettle(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	bs := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Hour)
	ctx := context.Background()
	pkOld, pkNew := "2025-09", "2025-10"

	// Reserve under the OLD period (period crossing at admission).
	_, _, w, _ := bs.ReserveScopeMarked(ctx, "key", "77", "monthly", pkOld, "req-t3", 4.0, 1000)
	require.True(t, w)
	s0 := snapDrift(t, rdb, "reserve pk_old e=4", "key", "77", pkOld)
	logSnaps(t, s0)
	require.InDelta(t, 0, s0.drift, 1e-9)

	// Settle under the NEW period (FINDING-B fallback path).
	bs.SettleScope(ctx, "key", "77", "monthly", pkNew, "req-t3", 5.0)
	so := snapDrift(t, rdb, "settle pk_new (old view)", "key", "77", pkOld)
	sn := snapDrift(t, rdb, "settle pk_new (new view)", "key", "77", pkNew)
	logSnaps(t, so, sn)

	// Force quiescence: reap any expired member.
	bs.ReapExpiredReservations(ctx, time.Now().Add(3*time.Hour))
	fo := snapDrift(t, rdb, "quiescent (old)", "key", "77", pkOld)
	fn := snapDrift(t, rdb, "quiescent (new)", "key", "77", pkNew)
	logSnaps(t, fo, fn)

	// Old period: its only activity was the reserve — the settle must have
	// released it (r=0, b=0), not stranded it.
	require.InDelta(t, 0, fo.r, 1e-9, "old-period resv must release on settle: %s", fo)
	// New period: settle exact (b==s==5, r=0).
	require.InDelta(t, 5, fn.s, 1e-9, "new settled: %s", fn)
	require.InDelta(t, 5, fn.b, 1e-9, "new budget: %s", fn)
	require.InDelta(t, 0, fn.r, 1e-9, "new resv: %s", fn)
}

// T4 — dual-instance: one instance settles while the other runs calibration
// rounds concurrently (cross-replica interleave). Invariant must hold at
// quiescence with resv=0 and budget==settled==ledger.
func TestDriftRepro_T4_DualInstanceSettleVsCalibration(t *testing.T) {
	_, _, mr, dbF := newCalibrationFixture(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cs := NewBudgetCalibrationService(dbF, rdb)
	bsA := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Hour)
	bsB := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Hour)
	ctx := context.Background()
	var tm struct{ ID int64 }
	require.NoError(t, dbF.Table("teams").Select("id").First(&tm).Error)
	id := fmt.Sprintf("%d", tm.ID)
	pk := PeriodKey("monthly")

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		req := fmt.Sprintf("req-t4-%d", i)
		_, _, w, _ := bsA.ReserveScopeMarked(ctx, "team", id, "monthly", pk, req, 1.0, 1000)
		require.True(t, w)
		wg.Add(2)
		go func(req string) { defer wg.Done(); bsB.SettleScope(ctx, "team", id, "monthly", pk, req, 1.0) }(req)
		go func() {
			defer wg.Done()
			// Concurrent calibration round with the ledger truth so far
			// (deterministic injection; values only grow).
			_ = cs.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", float64(i), 1000)
		}()
	}
	wg.Wait()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	// Final round with the full ledger truth (N rows of cost 1).
	require.NoError(t, cs.syncRedisAndSnapshot(ctx, "tok", "team", tm.ID, pk, "monthly", float64(N), 1000))
	q := snapDrift(t, rdb, "quiescent", "team", id, pk)
	logSnaps(t, q)
	quiescenceChecks(t, q, N, "T4")
}
