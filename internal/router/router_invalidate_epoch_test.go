package router

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M1 tests: the write-invalidation epoch barrier — an in-flight resolve that
// read the DB before an invalidation must not backfill stale mappings — plus
// targeted InvalidateModel key matching and the degraded negative-cache guard.

// gatedProviderModelRepo blocks the FIRST FindByModelName call on gate (after
// signaling entry), returning rows1; every later call returns rows2. This
// makes the resolve-vs-invalidate interleaving deterministic.
type gatedProviderModelRepo struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	gate    chan struct{}
	rows1   []model.ProviderModel
	rows2   []model.ProviderModel
	err1    error
}

func (m *gatedProviderModelRepo) FindByModelName(_ context.Context, _ string, _ int64) ([]model.ProviderModel, error) {
	m.mu.Lock()
	m.calls++
	first := m.calls == 1
	m.mu.Unlock()
	if first {
		if m.entered != nil {
			m.entered <- struct{}{}
		}
		if m.gate != nil {
			<-m.gate
		}
		if m.err1 != nil {
			return nil, m.err1
		}
		return m.rows1, nil
	}
	return m.rows2, nil
}

func (m *gatedProviderModelRepo) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func mapping(providerModel string) []model.ProviderModel {
	return []model.ProviderModel{{
		ID:            1,
		ProviderModel: providerModel,
		Weight:        1,
		Priority:      1,
		Status:        1,
		Provider:      model.Provider{Name: "p1", Status: 1},
	}}
}

func newEpochTestResolver(repo ProviderModelRepo) *Resolver {
	reg := provider.NewRegistry()
	reg.Register("p1", &mockProvider{name: "p1"})
	return NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
}

// TestResolve_NoStaleBackfillAfterInvalidate: a resolve that read the DB
// BEFORE an invalidation must not Store its stale result over the fresh
// generation — it re-resolves once on the new epoch instead.
func TestResolve_NoStaleBackfillAfterInvalidate(t *testing.T) {
	repo := &gatedProviderModelRepo{
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
		rows1:   mapping("old-upstream-model"),
		rows2:   mapping("new-upstream-model"),
	}
	r := newEpochTestResolver(repo)

	res := make(chan struct{})
	var routes []*RouteResult
	var err error
	go func() {
		defer close(res)
		routes, err = r.Resolve(context.Background(), "m", 0)
	}()

	<-repo.entered         // owner is inside the first DB query
	r.InvalidateModel("m") // bump epoch while the stale read is in flight
	close(repo.gate)       // release the stale result
	<-res

	require.NoError(t, err)
	require.NotEmpty(t, routes)
	assert.Equal(t, "new-upstream-model", routes[0].ProviderModel,
		"stale pre-invalidation mapping must not be served or cached")

	// The guarded store must have cached the RETRIED (fresh) generation.
	cached, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.Equal(t, "new-upstream-model", cached[0].ProviderModel)
	assert.Equal(t, 2, repo.callCount(), "exactly one re-resolve, no loop")
}

// TestResolve_DegradedNotWrittenAcrossGenerations: a DB failure observed by a
// pre-invalidation query must not suppress the next generation's lookups.
func TestResolve_DegradedNotWrittenAcrossGenerations(t *testing.T) {
	repo := &gatedProviderModelRepo{
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
		err1:    fmt.Errorf("%w: boom", ErrResolveDegraded),
		rows2:   mapping("fresh"),
	}
	r := newEpochTestResolver(repo)

	res := make(chan error, 1)
	go func() {
		_, err := r.Resolve(context.Background(), "m", 0)
		res <- err
	}()
	<-repo.entered
	r.InvalidateModel("m")
	close(repo.gate)
	require.ErrorIs(t, <-res, ErrResolveDegraded)

	r.degradedMu.Lock()
	_, suppressed := r.degraded["m:0"]
	r.degradedMu.Unlock()
	assert.False(t, suppressed, "old-generation failure must not negative-cache the new generation")

	// The next resolve queries the DB again (no suppression) and succeeds.
	routes, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	assert.Equal(t, 2, repo.callCount())
}

// TestInvalidateModel_ExactNameMatching: keys are "model:orgID"; matching
// parses at the LAST colon so colon-containing model names stay distinct.
func TestInvalidateModel_ExactNameMatching(t *testing.T) {
	r := newEpochTestResolver(&mockProviderModelRepo{data: nil})
	for _, k := range []string{"gpt-4:0", "gpt-4:7", "gpt-4o:0", "gpt:4:0", "other:0"} {
		r.cache.Store(k, &cacheEntry{results: nil, expire: time.Now().Add(time.Minute)})
	}
	before := r.epoch.Load()
	r.InvalidateModel("gpt-4")
	after := r.epoch.Load()
	assert.Greater(t, after, before, "invalidation must bump the epoch barrier")

	for _, k := range []string{"gpt-4:0", "gpt-4:7"} {
		_, ok := r.cache.Load(k)
		assert.False(t, ok, "%s should be invalidated", k)
	}
	for _, k := range []string{"gpt-4o:0", "gpt:4:0", "other:0"} {
		_, ok := r.cache.Load(k)
		assert.True(t, ok, "%s must survive (no bare-prefix matching)", k)
	}
}
