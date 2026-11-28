package router

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The resolver cache holds the PRE-selection pool, so a cache hit runs the
// routing strategy for its own request. These tests pin that contract: with a
// cached ordered list instead, every request inside the 30s TTL replayed the
// pick made by the cache-filling request (or, after P4.4b, the weight-sorted
// first route), and weighted_random / round_robin / canary had no per-request
// effect.

type reselectRepo struct {
	mu    sync.Mutex
	calls int
	data  map[string][]model.ProviderModel
}

func (m *reselectRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return m.data[name], nil
}

func (m *reselectRepo) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// scriptedStrategy puts a scripted provider first on each Select, counts the
// calls and remembers the candidates it was handed — so a test can tell
// per-request selection from a replayed order, and see what the strategy read.
type scriptedStrategy struct {
	mu    sync.Mutex
	calls int
	picks []string
	seen  []RouteCandidate
}

func (s *scriptedStrategy) Name() StrategyName { return StrategyWeightedRandom }

func (s *scriptedStrategy) Select(_ context.Context, candidates []RouteCandidate) (*RouteCandidate, []*RouteResult) {
	s.mu.Lock()
	want := s.picks[s.calls%len(s.picks)]
	s.calls++
	s.seen = append([]RouteCandidate(nil), candidates...)
	s.mu.Unlock()
	var first, rest []RouteCandidate
	for _, c := range candidates {
		if c.Provider.Name() == want && len(first) == 0 {
			first = append(first, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(first) == 0 {
		return nil, candidatesToRouteResults(candidates)
	}
	return &first[0], append(candidatesToRouteResults(first), candidatesToRouteResults(rest)...)
}

func (s *scriptedStrategy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// latencySeen returns the latency snapshot handed to the strategy for name on
// its latest Select.
func (s *scriptedStrategy) latencySeen(name string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.seen {
		if c.Provider.Name() == name {
			return c.AvgLatencyMs
		}
	}
	return -1
}

func mappingsRepo(strategy string, weightA, weightB int) *reselectRepo {
	return &reselectRepo{data: map[string][]model.ProviderModel{
		"m": {
			{ID: 1, ModelName: "m", ProviderModel: "a-model", Weight: weightA, Priority: 1, Status: 1, RoutingStrategy: strategy, Provider: model.Provider{Name: "a", Status: 1}},
			{ID: 2, ModelName: "m", ProviderModel: "b-model", Weight: weightB, Priority: 2, Status: 1, RoutingStrategy: strategy, Provider: model.Provider{Name: "b", Status: 1}},
		},
	}}
}

func twoProviderRegistry() *provider.Registry {
	reg := provider.NewRegistry()
	reg.Register("a", &mockProvider{name: "a"})
	reg.Register("b", &mockProvider{name: "b"})
	return reg
}

type countingLatency struct {
	calls atomic.Int32
	value atomic.Int64
}

func (l *countingLatency) GetAvgLatency(_ context.Context, _ string) float64 {
	l.calls.Add(1)
	return float64(l.value.Load())
}

// Every cache hit selects again: the first route follows the strategy's
// per-call script while the mapping lookup runs only once.
func TestResolver_CacheHitRunsStrategyPerRequest(t *testing.T) {
	repo := mappingsRepo("weighted_random", 100, 100)
	strategy := &scriptedStrategy{picks: []string{"a", "b"}}
	r := NewResolver(twoProviderRegistry(), repo, nil, map[StrategyName]RoutingStrategy{StrategyWeightedRandom: strategy}, nil, nil, nil, nil)

	for i := 0; i < 6; i++ {
		routes, err := r.Resolve(context.Background(), "m", 0)
		require.NoError(t, err)
		require.Len(t, routes, 2)
		assert.Equal(t, strategy.picks[i%2], routes[0].Provider.Name(), "resolve #%d", i+1)
	}
	assert.Equal(t, 1, repo.callCount(), "one mapping lookup; the rest are cache hits")
	assert.Equal(t, 6, strategy.callCount())
}

// weighted_random spreads cache hits by weight. A replayed (or weight-sorted)
// order would send everything to one route, as observed before the fix.
func TestResolver_CacheHitWeightedRandomDistributes(t *testing.T) {
	cases := []struct {
		name             string
		weightA, weightB int
		minA, maxA       float64 // share of "a" picks, as fractions
	}{
		{"equal weights split about half", 100, 100, 0.35, 0.65},
		{"30:70 weights split about 30:70", 30, 70, 0.20, 0.40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := mappingsRepo("weighted_random", tc.weightA, tc.weightB)
			r := NewResolver(twoProviderRegistry(), repo, nil, map[StrategyName]RoutingStrategy{StrategyWeightedRandom: &WeightedRandomStrategy{}}, nil, nil, nil, nil)

			const n = 2000
			picks := map[string]int{}
			for i := 0; i < n; i++ {
				routes, err := r.Resolve(context.Background(), "m", 0)
				require.NoError(t, err)
				picks[routes[0].Provider.Name()]++
			}
			assert.Equal(t, 1, repo.callCount())
			share := float64(picks["a"]) / n
			assert.Greater(t, share, tc.minA, "a picked %d of %d", picks["a"], n)
			assert.Less(t, share, tc.maxA, "a picked %d of %d", picks["a"], n)
		})
	}
}

// round_robin keeps rotating on cache hits (it degrades to weighted random
// without Redis, which still rules out a replayed order).
func TestResolver_CacheHitRoundRobinReselects(t *testing.T) {
	repo := mappingsRepo("round_robin", 100, 100)
	r := NewResolver(twoProviderRegistry(), repo, nil, map[StrategyName]RoutingStrategy{StrategyRoundRobin: &RoundRobinStrategy{}}, nil, nil, nil, nil)

	const n = 2000
	picks := map[string]int{}
	for i := 0; i < n; i++ {
		routes, err := r.Resolve(context.Background(), "m", 0)
		require.NoError(t, err)
		picks[routes[0].Provider.Name()]++
	}
	assert.Equal(t, 1, repo.callCount())
	assert.Len(t, picks, 2)
	for name, c := range picks {
		assert.Greater(t, c, n/4, "%s must receive a share of the traffic", name)
	}
}

// The latency / active-request snapshots a cache hit hands to the strategies
// are refreshed through a 2s per-provider cache: at most one burst per TTL,
// not two Redis reads per candidate per request.
func TestResolver_CacheHitRefreshesMetricsWithinTTL(t *testing.T) {
	repo := mappingsRepo("weighted_random", 100, 100)
	lat := &countingLatency{}
	lat.value.Store(10)
	strategy := &scriptedStrategy{picks: []string{"a"}}
	r := NewResolver(twoProviderRegistry(), repo, nil, map[StrategyName]RoutingStrategy{strategy.Name(): strategy}, lat, nil, nil, nil)

	resolve := func(step string) {
		routes, err := r.Resolve(context.Background(), "m", 0)
		require.NoError(t, err)
		require.NotEmpty(t, routes, step)
	}
	resolve("miss") // resolvePlan reads the metrics directly: 2 calls
	assert.EqualValues(t, 2, lat.calls.Load())
	assert.InDelta(t, 10, strategy.latencySeen("b"), 0, "the miss saw the live snapshot")

	lat.value.Store(20)
	resolve("first hit") // refreshRouteMetrics: one burst per provider: 2 calls
	assert.EqualValues(t, 4, lat.calls.Load())
	assert.InDelta(t, 20, strategy.latencySeen("b"), 0, "the first hit saw the refreshed snapshot")

	lat.value.Store(30)
	for i := 0; i < 5; i++ {
		resolve("hit inside the TTL")
	}
	assert.EqualValues(t, 4, lat.calls.Load(), "no further reads inside the 2s TTL")
	assert.InDelta(t, 20, strategy.latencySeen("b"), 0, "hits inside the TTL share the cached snapshot")
}
