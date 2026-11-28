package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cache-hit selection keeps the P4.4b guarantee — a health=0 primary is never
// first while a healthy one exists — now that the strategy runs per request.

func healthCandidate(name string, weight int) RouteCandidate {
	return RouteCandidate{
		Provider:      &mockProvider{name: name},
		ProviderModel: "m",
		ProviderRow:   &model.Provider{Name: name, Status: 1},
		Weight:        weight,
		Priority:      1,
		ModelName:     "m",
	}
}

func providerOrder(routes []*RouteResult) []string {
	out := make([]string, 0, len(routes))
	for _, rr := range routes {
		out = append(out, rr.Provider.Name())
	}
	return out
}

// A health=0 primary is demoted behind the healthy primary and a Weight=0
// fallback sorts last; the cached pool itself is left untouched.
func TestResolver_CacheHitDemotesUnhealthyPrimary(t *testing.T) {
	r := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	r.healthFn = func(name, _ string) float64 {
		if name == "B" {
			return 0 // demoted
		}
		return 1.0
	}
	pool := []RouteCandidate{healthCandidate("A", 5), healthCandidate("B", 5), healthCandidate("C", 0)}
	r.cache.Store("m:0", &cacheEntry{candidates: pool, expire: time.Now().Add(time.Minute)})

	got, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"A", "B", "C"}, providerOrder(got))
	// The demotion applies to this request only: the shared pool keeps B's weight.
	assert.Equal(t, 5, pool[1].Weight)
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

// When healthFn is nil, cache hit neither demotes nor reorders: the healthy
// pool is served in cached order.
func TestResolver_CacheHitNilHealthFnNoReorder(t *testing.T) {
	r := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	// r.healthFn is nil
	pool := []RouteCandidate{healthCandidate("A", 5), healthCandidate("B", 5)}
	r.cache.Store("m:0", &cacheEntry{candidates: pool, expire: time.Now().Add(time.Minute)})
	got, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"A", "B"}, providerOrder(got))
}

// CachedHealthScoreFn hands strategies the 2s-cached scorer (nil when no
// scorer is wired), so running them per request cannot fan out Redis reads.
func TestResolver_CachedHealthScoreFn(t *testing.T) {
	bare := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	assert.Nil(t, bare.CachedHealthScoreFn())

	var calls int32
	wired := NewResolver(nil, nil, nil, nil, nil, nil, nil, nil)
	wired.SetHealthScoreFn(func(_, _ string) float64 {
		atomic.AddInt32(&calls, 1)
		return 1.0
	})
	cached := wired.CachedHealthScoreFn()
	require.NotNil(t, cached)
	cached("zhipu", "m")
	cached("zhipu", "m")
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls))
}
