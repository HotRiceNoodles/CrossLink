package router

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockProviderModelRepo struct {
	data map[string][]model.ProviderModel
}

func (m *mockProviderModelRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	return m.data[name], nil
}

func TestResolver_Resolve_WeightedPick(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})
	reg.Register("qwen", &mockProvider{name: "qwen"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"claude-sonnet-4-20250514": {
				{
					ID:            1,
					ProviderModel: "deepseek-chat",
					Weight:        3,
					Priority:      1,
					Status:        1,
					InputPrice:    0.0014,
					OutputPrice:   0.0028,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
				{
					ID:            2,
					ProviderModel: "qwen-max",
					Weight:        1,
					Priority:      1,
					Status:        1,
					InputPrice:    0.002,
					OutputPrice:   0.006,
					Provider:      model.Provider{Name: "qwen", Status: 1},
				},
			},
		},
	}

	r := NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	results, err := r.Resolve(context.Background(), "claude-sonnet-4-20250514", 0)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	// Primary must be one of the two weighted providers
	assert.Contains(t, []string{"deepseek", "qwen"}, results[0].Provider.Name())
}

func TestResolver_Resolve_FallbackChain(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})
	reg.Register("zhipu", &mockProvider{name: "zhipu"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"claude-sonnet-4-20250514": {
				{
					ID:            1,
					ProviderModel: "deepseek-chat",
					Weight:        3,
					Priority:      1,
					Status:        1,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
				{
					ID:            2,
					ProviderModel: "glm-4-plus",
					Weight:        0,
					Priority:      2,
					Status:        1,
					Provider:      model.Provider{Name: "zhipu", Status: 1},
				},
			},
		},
	}

	r := NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	results, err := r.Resolve(context.Background(), "claude-sonnet-4-20250514", 0)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	// Primary (weight > 0) first
	assert.Equal(t, "deepseek", results[0].Provider.Name())
	assert.Equal(t, "deepseek-chat", results[0].ProviderModel)
	// Fallback (weight == 0) second
	assert.Equal(t, "zhipu", results[1].Provider.Name())
}

func TestResolver_Resolve_NoModel(t *testing.T) {
	reg := provider.NewRegistry()
	repo := &mockProviderModelRepo{data: map[string][]model.ProviderModel{}}

	r := NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	_, err := r.Resolve(context.Background(), "unknown-model", 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no provider found")
}

func TestResolver_Resolve_DisabledModel(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"claude-sonnet-4-20250514": {
				{
					ProviderModel: "deepseek-chat",
					Status:        2,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
			},
		},
	}

	r := NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	_, err := r.Resolve(context.Background(), "claude-sonnet-4-20250514", 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no active provider")
}

func TestResolver_ResolveSingle(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"claude-sonnet-4-20250514": {
				{
					ID:            1,
					ProviderModel: "deepseek-chat",
					Weight:        3,
					Status:        1,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
			},
		},
	}

	r := NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	result, err := r.ResolveSingle(context.Background(), "claude-sonnet-4-20250514", 0)
	require.NoError(t, err)
	assert.Equal(t, "deepseek", result.Provider.Name())
}

func TestResolver_Resolve_SkipsUnhealthyProvider(t *testing.T) {
	health := provider.NewHealthTracker()
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})
	reg.Register("qwen", &mockProvider{name: "qwen"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"test-model": {
				{
					ID:            1,
					ProviderModel: "deepseek-chat",
					Weight:        3,
					Status:        1,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
				{
					ID:            2,
					ProviderModel: "qwen-max",
					Weight:        1,
					Status:        1,
					Provider:      model.Provider{Name: "qwen", Status: 1},
				},
			},
		},
	}

	// Mark deepseek as failing
	for i := 0; i < 3; i++ {
		health.RecordFailure("deepseek")
	}

	r := NewResolver(reg, repo, health, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	results, err := r.Resolve(context.Background(), "test-model", 0)
	require.NoError(t, err)

	// deepseek should be skipped, only qwen returned
	assert.Len(t, results, 1)
	assert.Equal(t, "qwen", results[0].Provider.Name())
}

func TestResolver_Resolve_SkipsModelScopeCircuit(t *testing.T) {
	health := provider.NewHealthTracker()
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})
	reg.Register("qwen", &mockProvider{name: "qwen"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"test-model": {
				{
					ID:            1,
					ProviderModel: "deepseek-chat",
					Weight:        3,
					Status:        1,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
				{
					ID:            2,
					ProviderModel: "qwen-max",
					Weight:        1,
					Status:        1,
					Provider:      model.Provider{Name: "qwen", Status: 1},
				},
			},
		},
	}

	// Open a model-scope circuit on deepseek/deepseek-chat only — the account key
	// stays healthy, so this only filters when Resolve checks (provider, model).
	health.RecordPersistentFailure("deepseek", "deepseek-chat", "model", time.Hour)

	r := NewResolver(reg, repo, health, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	results, err := r.Resolve(context.Background(), "test-model", 0)
	require.NoError(t, err)

	// deepseek/deepseek-chat is filtered by the model-scope circuit; only qwen returns.
	assert.Len(t, results, 1)
	assert.Equal(t, "qwen", results[0].Provider.Name())
}

// Regression: Resolve must not claim the half-open probe lease. Pre-fix,
// Resolve's IsHealthyModel filter set probeInFlight on the expired circuit;
// the FallbackEngine's own IsHealthyModel gate then saw the lease, skipped the
// provider, and the deferred ClearProbe (inside the never-started attempt)
// never released it. Every later Resolve filtered the provider out and the
// model was permanently unroutable ("no available route for model") until
// process restart — even with the upstream healthy again.
func TestResolver_Resolve_HalfOpenDoesNotLeakProbeLease(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(3, 5*time.Millisecond)
	reg := provider.NewRegistry()
	reg.Register("deepseek", &mockProvider{name: "deepseek"})

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"test-model": {
				{
					ID:            1,
					ProviderModel: "deepseek-chat",
					Weight:        3,
					Status:        1,
					Provider:      model.Provider{Name: "deepseek", Status: 1},
				},
			},
		},
	}

	// Open the circuit, then let it expire → half-open.
	for i := 0; i < 3; i++ {
		health.RecordFailure("deepseek")
	}
	time.Sleep(10 * time.Millisecond)

	r := NewResolver(reg, repo, health, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, nil, nil, nil, nil)

	// First Resolve: the half-open route is returned (it becomes the probe).
	results, err := r.Resolve(context.Background(), "test-model", 0)
	require.NoError(t, err)
	require.Len(t, results, 1)

	// Resolve must NOT have claimed the probe lease: the FallbackEngine's
	// IsHealthyModel gate (fallback_engine.go) must still pass. Pre-fix this
	// returned false — the engine skipped the provider and the lease leaked.
	assert.True(t, health.IsHealthyModel("deepseek", "deepseek-chat"),
		"Resolve must not claim the half-open probe lease")

	// Subsequent Resolves must still see the route (no permanent deadlock).
	// Second call exercises the cached path; third exercises uncached after
	// clearing the lease the way the engine's deferred ClearProbe would.
	results, err = r.Resolve(context.Background(), "test-model", 0)
	require.NoError(t, err)
	assert.Len(t, results, 1)

	health.ClearProbe("deepseek", "deepseek-chat") // what attemptNonStream's defer does
	results, err = r.Resolve(context.Background(), "test-model", 0)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}
