package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// r34 suite: the Redis-PERSISTENT settle ledger (crash/restart-safe
// audit-replay), the reserve-after-settle guard, the settle-once guard TTL,
// the exact interleave that produced the r33c residue, and the calibration
// settled-raise that closes lost settles into the audit counter.

// Ledger member codec round-trip (and malformed rejection).
func TestLedgerMember_RoundTrip(t *testing.T) {
	task := settleRetryTask{
		scope: "key", targetID: "42", period: "monthly", pk: "2026-10",
		reqID: "req-abc", cost: 0.002512,
		deadline: time.Now().Add(settleRetryHorizon).Truncate(time.Second),
	}
	got, ok := parseLedgerMember(ledgerMember(task))
	if !ok {
		t.Fatal("round-trip parse failed")
	}
	if got.scope != task.scope || got.targetID != task.targetID ||
		got.period != task.period || got.pk != task.pk || got.reqID != task.reqID {
		t.Fatalf("fields mismatch: %+v", got)
	}
	if got.cost != task.cost {
		t.Fatalf("cost = %v, want %v", got.cost, task.cost)
	}
	if !got.deadline.Equal(task.deadline) {
		t.Fatalf("deadline = %v, want %v", got.deadline, task.deadline)
	}
	for _, bad := range []string{"", "only\x1ftwo", "a\x1fb\x1fc\x1fd\x1fe\x1fnotafloat\x1f100", "a\x1fb\x1fc\x1fd\x1fe\x1f1.0"} {
		if _, ok := parseLedgerMember(bad); ok {
			t.Fatalf("malformed member accepted: %q", bad)
		}
	}
}

// Backlog + restart: entries sitting in the ledger (written by a CRASHED
// process, simulated by direct ZADD) are recovered by a NEWLY constructed
// BudgetService — the once-per-process worker is re-armed on construction
// and drainSettleLedgerOnce drives the same idempotent settle.
func TestSettleLedger_BacklogDrainAfterRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	const n = 50
	ctx := context.Background()
	pk := PeriodKey("monthly")
	now := time.Now()
	for i := 0; i < n; i++ {
		task := settleRetryTask{
			scope: "key", targetID: "7", period: "monthly", pk: pk,
			reqID: fmt.Sprintf("req-ledger-%03d", i), cost: 0.5,
			deadline: now.Add(settleRetryHorizon),
		}
		if err := rdb.ZAdd(ctx, settleLedgerZSet, redis.Z{
			Score:  float64(now.Add(-time.Second).Unix()), // due now
			Member: ledgerMember(task),
		}).Err(); err != nil {
			t.Fatalf("zadd: %v", err)
		}
	}

	// "Restart": a fresh service instance on the same Redis.
	s := NewBudgetServiceWithMarkerTTL(rdb, time.Hour)
	s.drainSettleLedgerOnce()

	if card := rdb.ZCard(ctx, settleLedgerZSet).Val(); card != 0 {
		t.Fatalf("ledger not drained: %d entries left", card)
	}
	// All settles applied full cost (marks never landed — mode 0) and the
	// invariant budget == settled + resv holds exactly.
	bk, rk, sk := markedKeys("key", "7", pk)
	if got := getF(t, rdb, bk); got != 25 {
		t.Fatalf("budget = %v, want 25", got)
	}
	if got := getF(t, rdb, sk); got != 25 {
		t.Fatalf("settled = %v, want 25", got)
	}
	// resv was never touched by these mode-0 settles: key absent == 0.
	if rv, err := rdb.Get(ctx, rk).Float64(); err != nil && err != redis.Nil {
		t.Fatalf("GET resv: %v", err)
	} else if rv != 0 {
		t.Fatalf("resv = %v, want 0", rv)
	}
}

// Enqueue tiering: Redis up → tier 1 (ledger); Redis down → tier 2
// (in-process); Redis down AND in-process queue full → counted loss (false).
func TestSettleLedger_EnqueueTiers(t *testing.T) {
	// Tier 1: ledger write succeeds and the entry is due-recoverable.
	s, rdb, _ := newMarkedRedis(t)
	ok := s.enqueueSettleRetry(settleRetryTask{
		scope: "key", targetID: "9", period: "monthly", pk: PeriodKey("monthly"),
		reqID: "req-tier1", cost: 1.5, deadline: time.Now().Add(settleRetryHorizon),
	})
	if !ok {
		t.Fatal("tier-1 enqueue failed with Redis up")
	}
	s.drainSettleLedgerOnce()
	ctx := context.Background()
	if card := rdb.ZCard(ctx, settleLedgerZSet).Val(); card != 0 {
		t.Fatalf("tier-1 entry not drained: %d", card)
	}

	// Tier 2 / loss: a service pointing at a dead Redis port.
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { dead.Close() })
	ds := NewBudgetServiceWithMarkerTTL(dead, time.Hour)
	ok = ds.enqueueSettleRetry(settleRetryTask{
		scope: "key", targetID: "9", period: "monthly", pk: PeriodKey("monthly"),
		reqID: "req-tier2", cost: 1.5, deadline: time.Now().Add(settleRetryHorizon),
	})
	if !ok {
		t.Fatal("tier-2 (in-process) fallback failed with Redis down")
	}

	// Queue full + Redis down → false (counted loss; hourly DB raise owns it).
	for i := 0; i < settleRetryQueueCap; i++ {
		settleRetryQ <- settleRetryTask{scope: "key", targetID: "9", period: "monthly",
			pk: PeriodKey("monthly"), reqID: fmt.Sprintf("filler-%d", i), cost: 0,
			deadline: time.Now().Add(-time.Second)} // expired: drains instantly if a worker ever runs
	}
	t.Cleanup(func() {
		for len(settleRetryQ) > 0 {
			<-settleRetryQ
		}
	})
	ok = ds.enqueueSettleRetry(settleRetryTask{
		scope: "key", targetID: "9", period: "monthly", pk: PeriodKey("monthly"),
		reqID: "req-lost", cost: 2.5, deadline: time.Now().Add(settleRetryHorizon),
	})
	if ok {
		t.Fatal("enqueue reported success with Redis down and queue full (loss not counted)")
	}
}

// A reserve replay AFTER the request settled must NOT re-reserve (r33c
// residual class): the reserve script checks the settledmark and returns
// state without creating a mark or incrementing counters.
func TestReserve_AfterSettle_NoReReservation(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	req := "req-settled-1"

	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, req, 5, 1000); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	s.SettleScope(ctx, "key", "5", "monthly", pk, req, 7)

	// Replay of the SAME request (idempotent retry after response loss):
	spent, _, w, x := s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, req, 5, 1000)
	if w {
		t.Fatal("replay after settle reported a held reservation")
	}
	if x {
		t.Fatalf("replay after settle reported exceeded")
	}
	bk, rk, sk := markedKeys("key", "5", pk)
	if got := getF(t, rdb, bk); got != 7 {
		t.Fatalf("budget after replay = %v, want 7 (no re-reserve)", got)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv after replay = %v, want 0", got)
	}
	if got := getF(t, rdb, sk); got != 7 {
		t.Fatalf("settled = %v, want 7", got)
	}
	if spent != 7 {
		t.Fatalf("reported spent = %v, want 7", spent)
	}
	if rdb.Exists(ctx, resvMarkKey("key", "5", req)).Val() != 0 {
		t.Fatal("replay re-created a reservation mark")
	}
}

// The settle-once guard must OUTLIVE the reservation mark plus the retry
// horizon: a settledmark that expires with the mark lets a late re-drive
// double-settle the same request.
func TestSettle_GuardTTLOutlivesMarker(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	req := "req-guard-1"
	s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, req, 5, 1000)
	s.SettleScope(ctx, "key", "5", "monthly", pk, req, 7)

	markTTL := rdb.TTL(ctx, resvMarkKey("key", "5", req)).Val() // -2 (deleted) or ≤ markerTTL
	guardTTL := rdb.TTL(ctx, settledMarkKey("key", "5", req)).Val()
	wantMin := time.Hour + settleRetryHorizon // newMarkedRedis uses markerTTL 1h
	if guardTTL < wantMin-time.Minute {
		t.Fatalf("settledmark TTL = %v, want ≥ %v (mark TTL was %v)", guardTTL, wantMin, markTTL)
	}
}

// The r33c interleave, deterministic: reserve → mark expires mid-outage →
// settle applies FULL cost (mode 0, mark absent) → the client's idempotent
// retry re-reserves → the response re-settles. With the settledmark guard +
// reserve-side settledmark check + reaper, the end state is EXACT:
// budget == settled == true cost, resv == 0, no live marks. (Pre-fix this
// class double-counted 2c against one ledger row, or stranded +e.)
func TestExpiredMark_InterleaveStaysExact(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Second)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	req := "req-interleave-1"
	const e = 5.0
	const c = 7.0

	// 1. Admission reserves.
	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, req, e, 1000); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	// 2. Outage outlives the mark TTL: mark expires, no settle yet.
	mr.FastForward(3 * time.Second)
	// 3. Recovery settle (e.g. ledger replay): mark absent → full cost.
	s.SettleScope(ctx, "key", "5", "monthly", pk, req, c)
	// 4. The client's idempotent retry re-runs admission for the SAME request.
	s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, req, e, 1000)
	// 5. The retry's response settles again (duplicate settle → no-op).
	s.SettleScope(ctx, "key", "5", "monthly", pk, req, c)
	// 6. Reaper clears the expired zset entry (clock past the member score).
	if reaped := s.ReapExpiredReservations(ctx, time.Now().Add(5*time.Second)); reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}

	bk, rk, sk := markedKeys("key", "5", pk)
	shk := budgetShadowKey("key", "5", pk)
	// r78v: the stale hold is QUARANTINED (not refunded — the reaper cannot
	// attribute the strand), so budget = c + e with shadow = e.
	if got := getF(t, rdb, bk); got != c+e {
		t.Fatalf("budget = %v, want %v (cost + quarantined hold)", got, c+e)
	}
	if got := getF(t, rdb, shk); got != e {
		t.Fatalf("shadow = %v, want %v (quarantined estimate)", got, e)
	}
	// 7. Convergence: the row-aware near pass replays the row's settle —
	// duplicate mode → release() refunds the quarantine from the shadow
	// side — and the bill lands EXACTLY on the single ledger row.
	s.SettleScope(ctx, "key", "5", "monthly", pk, req, c)
	if got := getF(t, rdb, bk); got != c {
		t.Fatalf("budget = %v, want %v (exact, single bill)", got, c)
	}
	if got := getF(t, rdb, sk); got != c {
		t.Fatalf("settled = %v, want %v (mirrors ONE ledger row)", got, c)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv = %v, want 0", got)
	}
	if got := getF(t, rdb, shk); got != 0 {
		t.Fatalf("shadow = %v, want 0 (quarantine netted by the replay)", got)
	}
	if rdb.Exists(ctx, resvMarkKey("key", "5", req)).Val() != 0 {
		t.Fatal("reservation mark still live")
	}
}

// Calibration settled-raise: budgetset mirrors SUM(DB); lost settles leave
// it lagging; each round RAISES it to the fresh snapshot (never lowers — a
// settled above the ledger sum is a double-apply signal, preserved).
func TestCalibration_SettledRaiseHealsLostSettles(t *testing.T) {
	a, _, mr, db := newCalibrationFixture(t)
	rdb := a.rdb
	ctx := context.Background()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)

	var tm struct{ ID int64 }
	if err := db.Table("teams").Select("id").First(&tm).Error; err != nil {
		t.Fatalf("fixture team: %v", err)
	}
	pk := PeriodKey("monthly")
	bk, rk, sk := markedKeys("team", fmt.Sprintf("%d", tm.ID), pk)

	// Round 1 with DB sum 10 → migration seeds settled=10.
	if err := a.calibrateTargetSync(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 100); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	// Simulate a lost settle cohort: settled lags to 8.75 while DB says 10.
	rdb.Set(ctx, sk, 8.75, time.Hour)
	rdb.Set(ctx, bk, 10.0, time.Hour)
	rdb.Set(ctx, rk, 0.0, time.Hour)

	// Round 2 with the same DB sum: settled raised back to the ledger sum.
	if err := a.calibrateTargetSync(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 100); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if got := getF(t, rdb, sk); got != 10 {
		t.Fatalf("settled after raise = %v, want 10", got)
	}

	// Round 3 with a SMALLER snapshot (cannot legitimately happen — SUM is
	// monotone — but the raise-only rule must hold anyway: never lowered).
	if err := a.calibrateTargetSync(ctx, "tok", "team", tm.ID, pk, "monthly", 9.0, 100); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if got := getF(t, rdb, sk); got != 10 {
		t.Fatalf("settled lowered to %v, want 10 (raise-only)", got)
	}
	_ = mr
}

// Late-settle AFTER the reaper already removed the expired reservation:
// settle lands mode-0 (full cost) against a counter already restored by the
// reaper — the end state must still be exact (budget == settled == cost,
// resv == 0). This is the "marker TTL expired, settlement arrives later"
// recovery corner of the ledger.
func TestLateSettle_AfterReaper_Exact(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Second)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	req := "req-late-reap-1"
	const e = 5.0
	const c = 7.0

	s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, req, e, 1000)
	mr.FastForward(3 * time.Second) // mark expires mid-outage
	if n := s.ReapExpiredReservations(ctx, time.Now().Add(5*time.Second)); n != 1 {
		t.Fatalf("reap = %d, want 1", n)
	}
	// The ledger re-drives the settle AFTER the reaper ran:
	s.SettleScope(ctx, "key", "5", "monthly", pk, req, c)

	bk, rk, sk := markedKeys("key", "5", pk)
	if got := getF(t, rdb, bk); got != c {
		t.Fatalf("budget = %v, want %v (reaper restored e, settle added c)", got, c)
	}
	if got := getF(t, rdb, sk); got != c {
		t.Fatalf("settled = %v, want %v", got, c)
	}
	if rv, err := rdb.Get(ctx, rk).Float64(); err == nil && rv != 0 {
		t.Fatalf("resv = %v, want 0", rv)
	}
}

// DB AUDIT-REPLAY PROOF (r35 directive): the ledger (usage_logs) is the
// financial truth; Redis counters are derived data. From ARBITRARY
// under-count damage on all three counters — the terminal state of every
// loss tier (in-memory queue crash, ledger-write failure, settle horizon
// expiry) — one reaper pass (resv overhang removal, ZSET-backed) plus one
// calibration round (budget raise + settled raise from SUM(DB)) converges
// the triple EXACTLY to the ledger. This is convergence to DB truth, not
// invariant self-consistency: budget and settled are independently forced
// to the DB sum, and resv reaches 0 because every reservation was
// reserve-script-created (hence ZSET-backed, hence reapable).
func TestDBAuditReplay_ConvergesArbitraryUndercount(t *testing.T) {
	a, _, mr, db := newCalibrationFixture(t)
	rdb := a.rdb
	ctx := context.Background()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)

	// Ledger truth: usage_logs rows for the fixture team summing to 10.
	var tm struct{ ID int64 }
	if err := db.Table("teams").Select("id").First(&tm).Error; err != nil {
		t.Fatalf("fixture team: %v", err)
	}
	pk := PeriodKey("monthly")
	tk := fmt.Sprintf("%d", tm.ID)
	one := int64(1)
	rows := []model.UsageLog{
		{APIKeyID: &one, TeamID: &tm.ID, ModelUsed: "m", Cost: 4, CreatedAt: time.Now()},
		{APIKeyID: &one, TeamID: &tm.ID, ModelUsed: "m", Cost: 6, CreatedAt: time.Now()},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed ledger row: %v", err)
		}
	}

	// Damaged derived state: budget under by 5, settled under by 7, resv
	// holding a phantom overhang of 2 whose reservation marker EXPIRED (the
	// post-crash shape: counters written, recovery tasks lost).
	bk, rk, sk := markedKeys("team", tk, pk)
	rdb.Set(ctx, bk, 5.0, time.Hour)   // DB says 10
	rdb.Set(ctx, sk, 3.0, time.Hour)   // DB says 10
	rdb.Set(ctx, rk, 2.0, time.Hour)   // phantom overhang (mark long gone)
	// Its ZSET entry (reserve-created) is due for reaping:
	rdb.ZAdd(ctx, budgetResvExpZSet, redis.Z{Score: float64(time.Now().Add(-time.Minute).Unix()),
		Member: resvExpMember("team", tk, "req-crash-1", "monthly", pk, "2.000000000")})

	// Recovery pass 1: reaper removes the expired overhang.
	bs := NewBudgetServiceWithMarkerTTL(rdb, time.Hour)
	if n := bs.ReapExpiredReservations(ctx, time.Now()); n != 1 {
		t.Fatalf("reap = %d, want 1", n)
	}
	// Recovery pass 2: calibration round forces budget AND settled to SUM(DB).
	if err := a.calibrateTargetSync(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 100); err != nil {
		t.Fatalf("calibration round: %v", err)
	}

	// r78v: the phantom hold is UNPROVABLE (its recovery tasks were lost),
	// so the round raises settled/budget to the DB sum but the quarantine
	// rides along — budget = 10 + shadow(2). It is NOT converted into free
	// headroom; a late settle or certifying row round would net it.
	if got := getF(t, rdb, bk); got != 12 {
		t.Fatalf("budget after audit replay = %v, want 12 (DB sum 10 + quarantined 2)", got)
	}
	if got := getF(t, rdb, sk); got != 10 {
		t.Fatalf("settled after audit replay = %v, want 10 (DB sum)", got)
	}
	if got := getF(t, rdb, budgetShadowKey("team", tk, pk)); got != 2 {
		t.Fatalf("shadow after audit replay = %v, want 2 (unprovable hold kept)", got)
	}
	if got := getF(t, rdb, rk); got != 0 {
		t.Fatalf("resv after reaper = %v, want 0", got)
	}
	_ = mr
}

// Enqueue-tier failure chain: Redis down (ledger write fails) -> in-memory
// tier -> then the PROCESS dies before the in-memory queue drains. What
// survives is the RESERVATION state itself (mark + ZSET + counters in
// Redis) — prove the post-crash recovery: the expired reservation is
// reaped and the DB-raise closes the under-count. The recovery tasks were
// lost; the financial truth is not.
func TestCrashAfterMemoryFallback_RecoversViaReaperAndRaise(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Second)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	req := "req-mem-crash-1"
	const e = 3.0

	// Reservation landed; the settle goes through the FULL fallback chain:
	// stop miniredis (Redis down) so the ledger ZADD fails and the task sits
	// in the in-memory queue; the "crash" then discards it (the queue is
	// process state — nothing to drain).
	s.ReserveScopeMarked(ctx, "key", "8", "monthly", pk, req, e, 1000)
	mr.SetError("simulated redis outage")
	ok := s.enqueueSettleRetry(settleRetryTask{
		scope: "key", targetID: "8", period: "monthly", pk: pk,
		reqID: req, cost: e, deadline: time.Now().Add(settleRetryHorizon),
	})
	if !ok {
		t.Fatal("in-memory fallback enqueue failed with Redis down")
	}
	n := rdb.ZCard(ctx, settleLedgerZSet).Val()
	if n != 0 {
		t.Fatalf("ledger unexpectedly holds entries during outage: %d", n)
	}
	// "Process dies": the in-memory queue contents are lost by construction
	// (never drained here). Redis returns; the mark expires.
	mr.SetError("")
	mr.FastForward(3 * time.Second)

	// Post-crash recovery on a fresh instance: the reaper QUARANTINES the
	// expired hold (r78v). The in-memory task may be re-driven by the
	// re-armed tier worker (a fresh BudgetService construction re-arms it)
	// at any moment after Redis returns — whichever of the tier worker or
	// the explicit settle below lands first, the end state is the SAME
	// exact single bill (settledmark + shadowmark are idempotency guards).
	bs := NewBudgetServiceWithMarkerTTL(rdb, time.Hour)
	if reaped := bs.ReapExpiredReservations(ctx, time.Now().Add(5*time.Second)); reaped != 1 {
		t.Fatalf("reap = %d, want 1", reaped)
	}
	bk, rk, sk := markedKeys("key", "8", pk)
	// Modeled recovery: the settle task re-driven (tier-1 ledger replay /
	// WAL replay). Idempotent with any tier-worker racing us.
	bs.SettleScope(ctx, "key", "8", "monthly", pk, req, e)
	require.Eventually(t, func() bool {
		b, berr := rdb.Get(ctx, bk).Float64()
		r, _ := rdb.Get(ctx, rk).Float64()
		sh, _ := rdb.Get(ctx, budgetShadowKey("key", "8", pk)).Float64()
		return berr == nil && b == e && r == 0 && sh == 0
	}, 3*time.Second, 20*time.Millisecond, "quarantine must net to the exact single bill")
	if got := getF(t, rdb, sk); got != e {
		t.Fatalf("settled after recovery = %v, want %v (single bill)", got, e)
	}
	if rdb.Exists(ctx, resvMarkKey("key", "8", req)).Val() != 0 {
		t.Fatal("mark survived expiry")
	}
}

// FINDING-B regression (r36 MONITOR root cause): a settle whose scope has NO
// recorded period key (reserve failed in an outage window) must fall back to
// the CURRENT period key — never write to keys with an empty period suffix
// (orphan keys read by no one; under-counted the real counter by the whole
// reserve-failed cohort: 49x0.002992 = 0.146608 stranded on
// budgetset:key:<id>: in runs/r36-20261021-002521-fbrepro).
func TestSettle_EmptyPeriodKey_FallsBackToCurrentPeriod(t *testing.T) {
	s, rdb, _ := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	req := "req-fb-empty-pk-1"

	// Reserve FAILED shape: no mark, no PeriodKeys entry — settle with pk=""
	// (exactly what applyBudgetDelta passed before the fix).
	s.SettleScope(ctx, "key", "11", "monthly", "", req, 0.002992)

	bk, rk, sk := markedKeys("key", "11", pk)
	if got := getF(t, rdb, sk); got < 0.002991 || got > 0.002993 {
		t.Fatalf("settled on current-period key = %v, want the settle cost", got)
	}
	if got := getF(t, rdb, bk); got < 0.002991 || got > 0.002993 {
		t.Fatalf("budget on current-period key = %v, want the settle cost", got)
	}
	// No orphan empty-suffix keys may exist:
	for _, orphan := range []string{"budgetset:key:11:", "budget:key:11:", "budgetresv:key:11:"} {
		if rdb.Exists(ctx, orphan).Val() != 0 {
			t.Fatalf("orphan empty-period key created: %s", orphan)
		}
	}
	_ = rk
}
