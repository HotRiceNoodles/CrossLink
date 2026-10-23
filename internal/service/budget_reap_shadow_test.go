package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Shadow quarantine suite (r78v, ruling 2026-10-22): a TTL-expired
// reservation is a LEGAL refund but NOT free headroom — the crashed owner
// may already have consumed upstream, and nothing can prove it did not.
// Contract under test:
//
//	S1  reap keeps budget counting the hold (admission still sees it);
//	S2  a late settle nets actual cost against the quarantine exactly once;
//	S3  the calibration raise targets settled+resv+shadow (convergence may
//	    never convert a quarantine back into headroom);
//	S4  a duplicate settle whose cost was counted elsewhere (far cert)
//	    refunds the quarantine from the shadow side, exactly once;
//	S5  a certifying far page nets the row's shadowmark (consumption proven
//	    by the ledger row replaces the quarantined estimate);
//	S6  cross-period: the quarantine is released in the period it was HELD
//	    in; the cost lands in the settle's period;
//	invariant everywhere: budget == settled + resv + shadow.

// expireAndReap simulates the post-crash state: mark TTL lapsed, zset
// member due; returns the reap count.
func expireAndReap(t *testing.T, s *BudgetService, rdb *redis.Client, mr *miniredis.Miniredis, scope, id, req, period, pk string, e float64) int {
	t.Helper()
	ctx := context.Background()
	mr.Del(resvMarkKey(scope, id, req))
	member := resvExpMember(scope, id, req, period, pk, fmt.Sprintf("%.9f", e))
	if err := rdb.ZAdd(ctx, budgetResvExpZSet, redis.Z{
		Score:  float64(time.Now().Add(-time.Minute).Unix()),
		Member: member,
	}).Err(); err != nil {
		t.Fatalf("zadd: %v", err)
	}
	return s.ReapExpiredReservations(ctx, time.Now())
}

func shadowState(t *testing.T, rdb *redis.Client, scope, id, pk string) (b, rv, st, sh float64) {
	t.Helper()
	bk, rk, sk := markedKeys(scope, id, pk)
	soft := func(k string) float64 {
		v, err := rdb.Get(context.Background(), k).Float64()
		if err != nil {
			return 0
		}
		return v
	}
	b = soft(bk)
	rv = soft(rk)
	st = soft(sk)
	sh = soft(budgetShadowKey(scope, id, pk))
	return
}

// S1 — no free headroom: after the reaper quarantines a crashed request's
// hold, a fresh admission that would fit ONLY because of a refund must
// still be rejected. This is the exact r78s chassis shape (A dead, B admits
// on reopened budget) that the old blind refund allowed.
func TestReapShadow_S1_NoFreeHeadroom(t *testing.T) {
	s, rdb, mr := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")

	// A reserves 0.9 against a 1.0 limit, then crashes (mark expires).
	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, "req-a", 0.9, 1.0); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	if n := expireAndReap(t, s, rdb, mr, "key", "1", "req-a", "monthly", pk, 0.9); n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	b, rv, st, sh := shadowState(t, rdb, "key", "1", pk)
	if b != 0.9 || rv != 0 || sh != 0.9 {
		t.Fatalf("after reap budget=%v resv=%v settled=%v shadow=%v; want 0.9/0/0/0.9", b, rv, st, sh)
	}
	// B asks for 0.5: true spend is UNPROVEN, so admission must still see
	// 0.9 and reject (0.9+0.5 > 1.0). Old code refunded -> 0+0.5 -> 200.
	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, "req-b", 0.5, 1.0); w || !x {
		t.Fatalf("B admitted on quarantined headroom: written=%v exceeded=%v (must reject)", w, x)
	}
	// A smaller ask that fits WITH the quarantine held is fine.
	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "1", "monthly", pk, "req-b2", 0.05, 1.0); !w || x {
		t.Fatalf("small admission must pass: written=%v exceeded=%v", w, x)
	}
}

// S2 — the ledger re-drives A's settle AFTER the reaper quarantined it:
// budget nets to the actual cost exactly (c replaces e), shadow clears,
// and a replayed duplicate settle is a full no-op (shadowmark DEL guard).
func TestReapShadow_S2_LateSettleNetsExactlyOnce(t *testing.T) {
	s, rdb, mr := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	const e, c = 5.0, 7.0

	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "5", "monthly", pk, "req-late", e, 1000); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	if n := expireAndReap(t, s, rdb, mr, "key", "5", "req-late", "monthly", pk, e); n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	s.SettleScope(ctx, "key", "5", "monthly", pk, "req-late", c)

	b, rv, st, sh := shadowState(t, rdb, "key", "5", pk)
	if b != c || st != c || rv != 0 || sh != 0 {
		t.Fatalf("after late settle budget=%v settled=%v resv=%v shadow=%v; want %v/%v/0/0", b, st, rv, sh, c, c)
	}
	// Replay (journal replay / retry ledger double-drive): mode 2 no-op.
	mode, err := s.runSettleScript(ctx, settleRetryTask{scope: "key", targetID: "5", period: "monthly", pk: pk, reqID: "req-late", cost: c})
	if err != nil || mode != 2 {
		t.Fatalf("replay settle mode=%d err=%v, want 2/nil", mode, err)
	}
	b, rv, st, sh = shadowState(t, rdb, "key", "5", pk)
	if b != c || st != c || rv != 0 || sh != 0 {
		t.Fatalf("after replay budget=%v settled=%v resv=%v shadow=%v (double-clear!); want %v/%v/0/0", b, st, rv, sh, c, c)
	}
}

// S3 — the calibration raise must include shadow in its target: converging
// budget to settled+resv alone would convert the quarantine back into free
// headroom — the exact defect class the quarantine closes.
func TestReapShadow_S3_RaiseIncludesShadow(t *testing.T) {
	_, _, mr, dbF := newCalibrationFixture(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cs := NewBudgetCalibrationService(dbF, rdb)
	bs := NewBudgetServiceWithMarkerTTL(rdb, 2*time.Hour)
	ctx := context.Background()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	var tm struct{ ID int64 }
	if err := dbF.Table("teams").Select("id").First(&tm).Error; err != nil {
		t.Fatalf("team fixture: %v", err)
	}
	tid := fmt.Sprintf("%d", tm.ID)
	pk := PeriodKey("monthly")

	// Crash cohort: e=3 quarantined, then the DB raise runs (ledger says 10).
	if _, _, w, x := bs.ReserveScopeMarked(ctx, "team", tid, "monthly", pk, "req-s3", 3.0, 1000); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	if n := expireAndReap(t, bs, rdb, mr, "team", tid, "req-s3", "monthly", pk, 3.0); n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	if err := cs.calibrateTargetSync(ctx, "tok", "team", tm.ID, pk, "monthly", 10.0, 1000); err != nil {
		t.Fatalf("calibration round: %v", err)
	}
	b, rv, st, sh := shadowState(t, rdb, "team", tid, pk)
	if st != 10 {
		t.Fatalf("settled = %v, want 10 (ledger sum)", st)
	}
	if sh != 3 {
		t.Fatalf("shadow = %v, want 3 (raise must not consume the quarantine)", sh)
	}
	if b != 13 {
		t.Fatalf("budget = %v, want 13 = settled(10)+resv(0)+shadow(3)", b)
	}
	_ = rv
}

// S4 — duplicate settle after reap: the cost was already counted by another
// path (far certificate / first settler), so THIS owner's settle must
// refund the quarantine from the shadow side — exactly once, budget net
// exactly c.
func TestReapShadow_S4_DuplicateSettleRefundsShadow(t *testing.T) {
	s, rdb, mr := newMarkedRedis(t)
	ctx := context.Background()
	pk := PeriodKey("monthly")
	const e, c = 5.0, 7.0

	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "6", "monthly", pk, "req-dup", e, 1000); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	if n := expireAndReap(t, s, rdb, mr, "key", "6", "req-dup", "monthly", pk, e); n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	// The far certificate counts the row's full cost first (X6 shape).
	rdb.Set(ctx, settledMarkKey("key", "6", "req-dup"), "1", time.Hour)
	rdb.IncrByFloat(ctx, fmt.Sprintf("budget:key:6:%s", pk), c)
	rdb.IncrByFloat(ctx, budgetSettledKey("key", "6", pk), c)

	mode, err := s.runSettleScript(ctx, settleRetryTask{scope: "key", targetID: "6", period: "monthly", pk: pk, reqID: "req-dup", cost: c})
	if err != nil || mode != 2 {
		t.Fatalf("owner settle after cert mode=%d err=%v, want 2/nil", mode, err)
	}
	b, rv, st, sh := shadowState(t, rdb, "key", "6", pk)
	if b != c || st != c || sh != 0 || rv != 0 {
		t.Fatalf("after duplicate refund budget=%v settled=%v resv=%v shadow=%v; want %v/%v/0/0", b, st, rv, sh, c, c)
	}
}

// S5 — cert netting: a far page that proves a request's consumption
// (ledger row) releases its shadowmark quarantine so the proven cost
// replaces the quarantined estimate instead of stacking on it. Driven at
// the script level with the exact key layout the production caller builds.
func TestReapShadow_S5_FarCertNetsQuarantine(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	pk := PeriodKey("monthly")
	const e, c = 4.0, 6.0

	// Pre-state: ledger already says settled=20 (includes the row's c=6 via
	// a prior near pass), budget=20+... hold on — model the pure far shape:
	// settled=14 (pre-row), budget=14+3(resv live elsewhere)+4(shadow)=21.
	setKey := budgetSettledKey("key", "7", pk)
	rdb.Set(ctx, setKey, "14", time.Hour)
	rdb.Set(ctx, budgetResvKey("key", "7", pk), "3", time.Hour)
	rdb.Set(ctx, budgetShadowKey("key", "7", pk), fmt.Sprintf("%v", e), time.Hour)
	rdb.Set(ctx, "budget:key:7:"+pk, fmt.Sprintf("%v", 14.0+3.0+e), time.Hour)
	rdb.Set(ctx, shadowMarkKey("key", "7", "req-far"), fmt.Sprintf("%v|%s", e, pk), time.Hour)
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)

	keys := []string{calibrationLockKey,
		"budget:key:7:" + pk,
		budgetResvKey("key", "7", pk),
		budgetShadowKey("key", "7", pk),
		setKey,
		settledMarkKey("key", "7", "req-far"),
		"calibcert:key:7:req-far",
		shadowMarkKey("key", "7", "req-far")}
	argv := []interface{}{"tok", int(time.Hour.Seconds()), int((2*time.Hour + time.Hour).Seconds()), int((24 * 7 * time.Hour).Seconds()), "inc", 0.0, c}
	fresh, err := farConvergeScript.Run(ctx, rdb, keys, argv...).Int()
	if err != nil || fresh != 1 {
		t.Fatalf("far page fresh=%d err=%v, want 1/nil", fresh, err)
	}
	b, rv, st, sh := shadowState(t, rdb, "key", "7", pk)
	// Row counted: settled 14->20; quarantine netted: shadow 4->0; budget
	// = 20 + 3 + 0 = 23 (was 21: +6 row, -4 quarantine).
	if st != 20 || sh != 0 || rv != 3 {
		t.Fatalf("after cert settle=%v resv=%v shadow=%v; want 20/3/0", st, rv, sh)
	}
	if b != 23 {
		t.Fatalf("budget = %v, want 23 (proven cost replaced the estimate)", b)
	}
	if rdb.Exists(ctx, shadowMarkKey("key", "7", "req-far")).Val() != 0 {
		t.Fatal("shadowmark must be consumed exactly once")
	}
	// A second far round over the same row: cert blanks the increment and
	// the netted quarantine must NOT double-release.
	if _, err := farConvergeScript.Run(ctx, rdb, keys, argv...).Int(); err != nil {
		t.Fatalf("second far round: %v", err)
	}
	b, rv, st, sh = shadowState(t, rdb, "key", "7", pk)
	if b != 23 || st != 20 || sh != 0 || rv != 3 {
		t.Fatalf("after re-cert budget=%v settled=%v resv=%v shadow=%v; want 23/20/3/0 (idempotent)", b, st, rv, sh)
	}
}

// S6 — cross-period: the hold was reserved (and quarantined) in pk_old; the
// late settle carries pk_new. The quarantine must release in pk_old and the
// cost land in pk_new (the drift-T3 rule, shadow side).
func TestReapShadow_S6_CrossPeriodQuarantine(t *testing.T) {
	s, rdb, mr := newMarkedRedis(t)
	ctx := context.Background()
	pkOld, pkNew := "2099-09", "2099-10"
	const e, c = 4.0, 5.0

	if _, _, w, x := s.ReserveScopeMarked(ctx, "key", "77", "monthly", pkOld, "req-x", e, 1000); !w || x {
		t.Fatalf("reserve: written=%v exceeded=%v", w, x)
	}
	if n := expireAndReap(t, s, rdb, mr, "key", "77", "req-x", "monthly", pkOld, e); n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	bo, ro, so, sho := shadowState(t, rdb, "key", "77", pkOld)
	if bo != e || ro != 0 || sho != e || so != 0 {
		t.Fatalf("old period after reap b=%v r=%v s=%v sh=%v; want %v/0/0/%v", bo, ro, so, sho, e, e)
	}
	s.SettleScope(ctx, "key", "77", "monthly", pkNew, "req-x", c)
	bo, ro, so, sho = shadowState(t, rdb, "key", "77", pkOld)
	bn, rn, sn, shn := shadowState(t, rdb, "key", "77", pkNew)
	if bo != 0 || ro != 0 || sho != 0 {
		t.Fatalf("old period after settle b=%v r=%v sh=%v; want 0/0/0 (quarantine released where held)", bo, ro, sho)
	}
	if bn != c || sn != c || rn != 0 || shn != 0 {
		t.Fatalf("new period after settle b=%v s=%v r=%v sh=%v; want %v/%v/0/0", bn, sn, rn, shn, c, c)
	}
}
