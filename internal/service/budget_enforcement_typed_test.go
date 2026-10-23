package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// r78u review: the fail-closed admission must surface as a TYPED error
// (503 budget_enforcement_unavailable at the handler), never masquerade as
// a quota 429; the boundary covers EVERY atomic-reserve failure (a plain
// GET re-check is check-then-act with no estimate deduction — concurrent
// requests would all pass it); and a real short Redis outage must reject
// ALL concurrent (multi-replica-shaped) admissions with zero phantom
// holds, recovering exactly when the store does.
func TestReserveE_TypedErrorNotQuota(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetService(rdb)
	scopes := []BudgetScope{{Scope: "team", ID: "21", Period: "monthly", Limit: 10}}
	// Healthy: fresh counter (redis.Nil at store level) ADMITS — the
	// reserve script creates the counter; a missing counter is spent=0,
	// categorically different from an unreachable store.
	if _, exc, err := s.ReserveForRequestE(context.Background(), scopes, 1.0, "te-ok"); exc != "" || err != nil {
		t.Fatalf("healthy fresh-counter admission failed: exc=%q err=%v", exc, err)
	}
	// Outage: typed enforcement error, NOT a quota verdict.
	mr.SetError("simulated outage")
	_, exc, err := s.ReserveForRequestE(context.Background(), scopes, 1.0, "te-down")
	if !errors.Is(err, ErrBudgetEnforcementUnavailable) {
		t.Fatalf("outage must return ErrBudgetEnforcementUnavailable, got err=%v exc=%q", err, exc)
	}
	// Legacy 4-tuple wrapper keeps compiling callers working (scope
	// reported exceeded) — product path never uses it.
	mr.SetError("")
	mr.SetError("outage2")
	if _, exc2 := s.ReserveForRequest(context.Background(), scopes, 1.0, "te-wrap"); exc2 == "" {
		t.Fatalf("legacy wrapper must surface outage as exceeded-scope for old callers")
	}
	mr.SetError("")
}

// Multi-replica concurrency during a short outage: every admission is
// rejected, nothing lands (no estimate holds, no counter movement), and
// the store recovers cleanly to exact accounting.
func TestReserveE_ShortOutageConcurrentNoAdmission(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewBudgetService(rdb)
	scopes := []BudgetScope{
		{Scope: "key", ID: "31", Period: "monthly", Limit: 100},
		{Scope: "team", ID: "32", Period: "monthly", Limit: 100},
		{Scope: "org", ID: "33", Period: "monthly", Limit: 100},
	}
	// Pre-outage: one admitted request leaves exact counters.
	if _, exc, err := s.ReserveForRequestE(context.Background(), scopes, 2.0, "co-pre"); exc != "" || err != nil {
		t.Fatalf("pre-outage reserve: exc=%q err=%v", exc, err)
	}
	snap := map[string]string{}
	for _, sc := range scopes {
		v, _ := rdb.Get(context.Background(), "budget:"+sc.Scope+":"+sc.ID+":2026-10").Result()
		snap[sc.Scope] = v
	}
	// Short outage: 32 concurrent admission attempts ("replicas") —
	// ALL must fail closed; zero of them may hold or move anything.
	mr.SetError("short outage")
	var wg sync.WaitGroup
	rejected, admitted := int32(0), int32(0)
	var mu sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, exc, err := s.ReserveForRequestE(context.Background(), scopes, 3.0, time.Now().Format("150405.000000000")+string(rune('a'+i%26)))
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrBudgetEnforcementUnavailable) && exc != "" {
				rejected++
			} else if err == nil && exc == "" {
				admitted++
			}
		}(i)
	}
	wg.Wait()
	mr.SetError("")
	if rejected != 32 || admitted != 0 {
		t.Fatalf("outage admissions: rejected=%d admitted=%d, want 32/0", rejected, admitted)
	}
	for _, sc := range scopes {
		v, _ := rdb.Get(context.Background(), "budget:"+sc.Scope+":"+sc.ID+":2026-10").Result()
		if v != snap[sc.Scope] {
			t.Fatalf("%s counter moved during outage: %v -> %v", sc.Scope, snap[sc.Scope], v)
		}
	}
	// Post-recovery: admission works and accounting is exact (pre 2.0 + new 3.0).
	if _, exc, err := s.ReserveForRequestE(context.Background(), scopes, 3.0, "co-post"); exc != "" || err != nil {
		t.Fatalf("post-recovery reserve: exc=%q err=%v", exc, err)
	}
	b, _ := rdb.Get(context.Background(), "budget:key:31:2026-10").Float64()
	if b != 5.0 {
		t.Fatalf("post-recovery budget=%v want 5.0 (2+3, no outage leakage)", b)
	}
}
