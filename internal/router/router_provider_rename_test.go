package router

import (
	"context"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M1 acceptance gap: the provider-rename scenario — Update allows changing
// Provider.Name, serveFromCache never re-checks the registry, so cached routes
// keep holding the OLD provider instance until TTL. The M1 wiring answers this
// with OnProviderApplied → Resolver.Invalidate() (full flush); this test pins
// both halves: the hazard exists without the flush, and the flush clears it.

type swapRepo struct {
	data map[string][]model.ProviderModel
}

func (m *swapRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	return m.data[name], nil
}

func TestResolver_ProviderRenameFlushesRoutes(t *testing.T) {
	t.Parallel()

	reg := provider.NewRegistry()
	instanceA := &mockProvider{name: "old-name"}
	instanceB := &mockProvider{name: "new-name"}
	reg.Register("old-name", instanceA)

	repo := &swapRepo{data: map[string][]model.ProviderModel{
		"m": {{
			ID: 1, ModelName: "m", ProviderModel: "pm", Weight: 1, Priority: 1, Status: 1,
			Provider: model.Provider{Name: "old-name", Status: 1, ID: 1},
		}},
	}}
	r := NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)

	// Fill the cache with the pre-rename route.
	routes, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	require.Same(t, instanceA, routes[0].Provider)

	// Rename: the old registry entry is gone (Update replaced the row under a
	// new name) and the mapping now points at the new name.
	reg.Remove("old-name")
	reg.Register("new-name", instanceB)
	repo.data["m"] = []model.ProviderModel{{
		ID: 1, ModelName: "m", ProviderModel: "pm", Weight: 1, Priority: 1, Status: 1,
		Provider: model.Provider{Name: "new-name", Status: 1, ID: 1},
	}}

	// HAZARD (pinned): without the flush, a cache hit still serves the OLD
	// instance — serveFromCache does not re-check the registry.
	cached, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	require.Same(t, instanceA, cached[0].Provider,
		"pre-flush cache hit must demonstrably hold the stale instance — this is the hazard M1 wiring exists to close")

	// The M1 wiring (RegistrySync.OnProviderApplied → Invalidate) clears it.
	r.Invalidate()
	fresh, err := r.Resolve(context.Background(), "m", 0)
	require.NoError(t, err)
	assert.Same(t, instanceB, fresh[0].Provider, "post-flush resolve must pick up the renamed provider")
}
