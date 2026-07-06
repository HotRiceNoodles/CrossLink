package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Cache-hit path reorders by effective weight: a healthy primary sorts first,
// a health=0 primary (demoted) and a Weight=0 fallback sort last.
func TestResolver_CacheHitReorderByHealth(t *testing.T) {
	r := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	r.healthFn = func(name, _ string) float64 {
		if name == "B" {
			return 0 // demoted
		}
		return 1.0
	}
	routes := []*RouteResult{
		{Provider: &mockProvider{name: "A"}, ProviderModel: "m", Weight: 5, Priority: 1},
		{Provider: &mockProvider{name: "B"}, ProviderModel: "m", Weight: 5, Priority: 1}, // health 0
		{Provider: &mockProvider{name: "C"}, ProviderModel: "m", Weight: 0, Priority: 1}, // fallback
	}
	r.cache.Store("m:0", &cacheEntry{results: routes, expire: time.Now().Add(time.Minute)})

	got, err := r.Resolve(context.Background(), "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(got))
	}
	// A (eff 5) must be first; B (demoted, eff 0) must NOT be first.
	if got[0].Provider.Name() != "A" {
		t.Errorf("expected healthy primary A first, got %s", got[0].Provider.Name())
	}
	if got[1].Provider.Name() == "B" && got[2].Provider.Name() == "B" {
		t.Errorf("B should be demoted")
	}
	// B (eff 0) must come after A.
	for i, rr := range got {
		if rr.Provider.Name() == "B" && i == 0 {
			t.Errorf("demoted B must not be first")
		}
	}
}

// cachedHealthScore caches the score for 2s: healthFn is called once for two
// lookups within the TTL window.
func TestResolver_CachedHealthScoreCachesWithinTTL(t *testing.T) {
	r := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	var calls int32
	r.healthFn = func(_, _ string) float64 {
		atomic.AddInt32(&calls, 1)
		return 1.0
	}
	first := r.cachedHealthScore("zhipu", "m")
	second := r.cachedHealthScore("zhipu", "m")
	if first != second {
		t.Errorf("expected equal scores, got %v %v", first, second)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected healthFn called once within TTL, got %d", calls)
	}
}

// When healthFn is nil, cache-hit returns results in cached order (no reorder).
func TestResolver_CacheHitNilHealthFnNoReorder(t *testing.T) {
	r := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	// r.healthFn is nil
	routes := []*RouteResult{
		{Provider: &mockProvider{name: "A"}, ProviderModel: "m", Weight: 5},
		{Provider: &mockProvider{name: "B"}, ProviderModel: "m", Weight: 5},
	}
	r.cache.Store("m:0", &cacheEntry{results: routes, expire: time.Now().Add(time.Minute)})
	got, err := r.Resolve(context.Background(), "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Provider.Name() != "A" {
		t.Errorf("nil healthFn should preserve cached order: %v", got)
	}
}
