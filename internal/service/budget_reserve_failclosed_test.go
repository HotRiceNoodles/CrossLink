package service

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// R78q review: with budget limits configured, a reserve whose Redis paths
// are ALL unavailable must fail CLOSED — the old fallback treated an
// unreadable counter as spent=0 and admitted, which is unbounded
// over-admission for the whole outage window on every replica (r78q: the
// 8 delivered-but-unreserved requests during CLIENT PAUSE).
func TestReserve_FailClosedWhenCounterStoreUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetService(rdb)
	// A live reservation proves the happy path admits.
	scopes := []BudgetScope{{Scope: "team", ID: "9", Period: "monthly", Limit: 10}}
	if _, exc := s.ReserveForRequest(context.Background(), scopes, 1.0, "fc-ok"); exc != "" {
		t.Fatalf("healthy store must admit, got exceeded=%q", exc)
	}
	// Kill the store: admission must fail closed (scope reported exceeded).
	mr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, exc := s.ReserveForRequest(ctx, scopes, 1.0, "fc-down")
	if exc == "" {
		t.Fatalf("counter store unavailable: admission ADMITTED (want fail-closed exceeded scope)")
	}
}

// The settle-side duplicate release (X6) at service level: a reservation
// that outlives a far certificate must be refunded when its settle lands.
func TestSettle_DuplicateReleasesLingeringReservation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetService(rdb)
	ctx := context.Background()
	// Reserve e=2 for reqX.
	if _, _, w, x := s.ReserveScopeMarked(ctx, "team", "10", "monthly", "2026-10", "dup-1", 2.0, 1000); !w || x {
		t.Fatalf("reserve failed")
	}
	// Someone else certified reqX (far convergence): settledmark present.
	if err := rdb.Set(ctx, settledMarkKey("team", "10", "dup-1"), "1", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	// The owner's settle now duplicates: must release e, not strand it.
	if code, err := s.runSettleScript(ctx, settleRetryTask{
		scope: "team", targetID: "10", period: "monthly", pk: "2026-10",
		reqID: "dup-1", cost: 5.0,
	}); err != nil || code != 2 {
		t.Fatalf("settle code=%d err=%v want duplicate(2)", code, err)
	}
	rv, err := rdb.Get(ctx, "budgetresv:team:10:2026-10").Result()
	if err == nil && rv != "0" {
		t.Fatalf("resv=%q want released-to-0 (or nil)", rv)
	}
	if rdb.Exists(ctx, resvMarkKey("team", "10", "dup-1")).Val() != 0 {
		t.Fatalf("resvmark must be deleted on duplicate settle")
	}
}
