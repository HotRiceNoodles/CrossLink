package service

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newBudgetTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb, mr
}

// TestReserveBudget_RollsBackOnExceed verifies the Lua script refunds its own
// increment when a reservation would exceed the limit, so rejected requests
// don't permanently inflate the budget counter.
func TestReserveBudget_RollsBackOnExceed(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	s := NewBudgetService(rdb)
	ctx := context.Background()

	// Pre-seed spent=9 on a $10 key budget.
	s.ReportUsage(ctx, "key", "1", "monthly", 9.0)

	// Reserve 2 -> would hit 11 >= 10 -> exceeded, rolled back.
	spent, _, exceeded := s.ReserveBudget(ctx, "key", "1", "monthly", 2.0, 10.0)
	if !exceeded {
		t.Fatal("expected exceeded")
	}
	if spent != 9.0 {
		t.Fatalf("expected spent rolled back to 9, got %v", spent)
	}
	if got := s.GetCurrentSpent(ctx, "key", "1", "monthly"); got != 9.0 {
		t.Fatalf("expected counter unchanged at 9 after rollback, got %v", got)
	}

	// Reserve 0.5 -> 9.5 < 10 -> ok, counter advances.
	if _, _, exceeded := s.ReserveBudget(ctx, "key", "1", "monthly", 0.5, 10.0); exceeded {
		t.Fatal("expected reservation within limit to succeed")
	}
	if got := s.GetCurrentSpent(ctx, "key", "1", "monthly"); got != 9.5 {
		t.Fatalf("expected counter 9.5 after ok reserve, got %v", got)
	}
}

// TestReserveForRequest_MultiLevelRollback verifies that when a higher-level
// scope rejects, all lower-level reservations are refunded and the returned
// reservations are empty (so ReportBudgetUsage won't double-reconcile).
func TestReserveForRequest_MultiLevelRollback(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	s := NewBudgetService(rdb)
	ctx := context.Background()

	// key (limit 10) and org (limit 100) can absorb a 2.0 reserve; team (limit 1)
	// cannot — it should reject and refund key.
	scopes := []BudgetScope{
		{Scope: "key", ID: "1", Period: "monthly", Limit: 10},
		{Scope: "team", ID: "5", Period: "monthly", Limit: 1},
		{Scope: "org", ID: "9", Period: "monthly", Limit: 100},
	}
	res, exceeded := s.ReserveForRequest(ctx, scopes, 2.0)
	if exceeded != "team" {
		t.Fatalf("expected exceeded at team, got %q", exceeded)
	}
	for scope, id := range map[string]string{"key": "1", "team": "5", "org": "9"} {
		if got := s.GetCurrentSpent(ctx, scope, id, "monthly"); got != 0 {
			t.Errorf("%s counter should be 0 after rollback, got %v", scope, got)
		}
	}
	if len(res.Reserved) != 0 {
		t.Errorf("expected no held reservations after rollback, got %v", res.Reserved)
	}
}

// TestReserveForRequest_SuccessThenReconcile verifies a full reserve+reconcile
// cycle leaves each counter at the actual cost (not actual+reserve).
func TestReserveForRequest_SuccessThenReconcile(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	s := NewBudgetService(rdb)
	ctx := context.Background()

	scopes := []BudgetScope{
		{Scope: "key", ID: "1", Period: "monthly", Limit: 10},
		{Scope: "team", ID: "5", Period: "monthly", Limit: 100},
	}
	res, exceeded := s.ReserveForRequest(ctx, scopes, 2.0)
	if exceeded != "" {
		t.Fatalf("expected no exceed, got %q", exceeded)
	}
	// After reserve: both counters hold 2.0.
	if got := s.GetCurrentSpent(ctx, "key", "1", "monthly"); got != 2.0 {
		t.Fatalf("key after reserve = %v, want 2.0", got)
	}

	// Reconcile: actual cost 1.5 -> delta = 1.5 - 2.0 = -0.5 per reserved level.
	for _, sc := range scopes {
		s.AdjustBudget(ctx, sc.Scope, sc.ID, sc.Period, 1.5-res.Reserved[sc.Scope])
	}
	if got := s.GetCurrentSpent(ctx, "key", "1", "monthly"); got != 1.5 {
		t.Fatalf("key after reconcile = %v, want 1.5", got)
	}
	if got := s.GetCurrentSpent(ctx, "team", "5", "monthly"); got != 1.5 {
		t.Fatalf("team after reconcile = %v, want 1.5", got)
	}
}

// TestReserveForRequest_ZeroEstimateNoop verifies a zero/negative estimate
// reserves nothing (prices unknown — actual still reported post-response).
func TestReserveForRequest_ZeroEstimateNoop(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	s := NewBudgetService(rdb)
	ctx := context.Background()

	scopes := []BudgetScope{{Scope: "key", ID: "1", Period: "monthly", Limit: 10}}
	res, exceeded := s.ReserveForRequest(ctx, scopes, 0)
	if exceeded != "" {
		t.Fatalf("expected no exceed on zero estimate, got %q", exceeded)
	}
	if got := s.GetCurrentSpent(ctx, "key", "1", "monthly"); got != 0 {
		t.Fatalf("expected no reservation, got %v", got)
	}
	if len(res.Reserved) != 0 {
		t.Errorf("expected empty reservations, got %v", res.Reserved)
	}
}
