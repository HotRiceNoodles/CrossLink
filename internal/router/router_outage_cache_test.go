package router

import (
	"context"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRepo counts FindByModelName calls to prove the cache serves the
// rejection WITHOUT touching the DB.
type countingRepo struct {
	data  map[string][]model.ProviderModel
	calls int
}

func (m *countingRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	m.calls++
	return m.data[name], nil
}

// Outage behavior (soak r2 finding): when every cached route is circuit-open,
// Resolve must serve the ErrNoActiveProvider rejection FROM THE CACHE. The
// previous fallthrough re-queried provider_models per rejected request — an
// upstream outage at 300rps became a DB query storm (178k slow queries
// saturated Postgres and cascaded into resolve-degraded errors).
func TestResolver_OutageServesRejectionFromCache_NoDBStorm(t *testing.T) {
	health := provider.NewHealthTracker()
	reg := provider.NewRegistry()
	reg.Register("zhipu", &mockProvider{name: "zhipu"})
	repo := &countingRepo{data: map[string][]model.ProviderModel{
		"m": {
			{ID: 1, ProviderModel: "glm-a", Weight: 1, Status: 1,
				Provider: model.Provider{Name: "zhipu", Status: 1}},
		},
	}}
	r := NewResolver(reg, repo, health, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)

	// Warm the cache with a successful resolve.
	_, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	require.Equal(t, 1, repo.calls)

	// Trip the circuit (account-level, as a network outage would).
	for i := 0; i < 3; i++ {
		health.RecordTransientFailure("zhipu", "glm-a", 0)
	}

	// A burst of rejected requests must NOT query the DB again.
	for i := 0; i < 100; i++ {
		_, err := r.Resolve(context.Background(), "m", 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNoActiveProvider)
	}
	assert.Equal(t, 1, repo.calls, "rejections must be served from cache, not per-request DB queries")

	// After the circuit closes (success), traffic resumes without a DB hit
	// (cache still warm) — and a mapping edit path (Invalidate) still forces
	// a fresh query when needed.
	health.RecordSuccessModel("zhipu", "glm-a")
	routes, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.NotEmpty(t, routes)
	assert.Equal(t, 1, repo.calls)

	r.Invalidate()
	_, err = r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.Equal(t, 2, repo.calls, "after Invalidate the next resolve re-queries")
}
