package router

import (
	"context"
	"errors"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 503 semantics: a model that EXISTS but whose every route was filtered out
// (circuit-open, disabled, tier-gated) must carry ErrNoActiveProvider so the
// handler maps it to 503; a genuinely unknown model keeps the plain error (404).
func TestResolver_Resolve_NoActiveProviderSentinel(t *testing.T) {
	health := provider.NewHealthTracker()
	reg := provider.NewRegistry()
	reg.Register("zhipu", &mockProvider{name: "zhipu"})
	strategies := map[StrategyName]RoutingStrategy{StrategyWeightedRandom: &WeightedRandomStrategy{}}

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"blocked": {
				{ID: 1, ProviderModel: "glm-a", Weight: 1, Status: 1,
					Provider: model.Provider{Name: "zhipu", Status: 1}},
			},
		},
	}
	r := NewResolver(reg, repo, health, strategies, nil, nil, nil, nil)

	// Before the circuit opens the model resolves fine (cache warm).
	_, err := r.Resolve(context.Background(), "blocked", 0)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		health.RecordPersistentFailure("zhipu", "glm-a", "model", 0)
	}
	_, err = r.Resolve(context.Background(), "blocked", 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoActiveProvider)

	// Unknown model: plain error, NOT the sentinel -> stays 404.
	_, err = r.Resolve(context.Background(), "does-not-exist", 0)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoActiveProvider), "unknown model must not look temporarily-unavailable")
	assert.Contains(t, err.Error(), "no provider found for model: does-not-exist")
}

// Isolation regression (soak1: 16,779 collateral rejections): a sibling
// model's rate-limit circuit must not filter this model's routes — only the
// account-wide (network/5xx) circuit may.
func TestResolver_Resolve_SiblingRateLimitDoesNotBlockModel(t *testing.T) {
	health := provider.NewHealthTracker()
	reg := provider.NewRegistry()
	reg.Register("zhipu", &mockProvider{name: "zhipu"})
	strategies := map[StrategyName]RoutingStrategy{StrategyWeightedRandom: &WeightedRandomStrategy{}}

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"healthy": {
				{ID: 1, ProviderModel: "glm-b", Weight: 1, Status: 1,
					Provider: model.Provider{Name: "zhipu", Status: 1}},
			},
		},
	}
	r := NewResolver(reg, repo, health, strategies, nil, nil, nil, nil)

	for i := 0; i < 5; i++ {
		health.RecordRateLimitFailure("zhipu", "glm-a", 0) // sibling model 429s
	}
	routes, err := r.Resolve(context.Background(), "healthy", 0)
	require.NoError(t, err, "sibling model rate limits must not make healthy model unroutable")
	assert.NotEmpty(t, routes)

	// But an account-wide failure (network/5xx) legitimately blocks it.
	for i := 0; i < 3; i++ {
		health.RecordTransientFailure("zhipu", "glm-a", 0)
	}
	_, err = r.Resolve(context.Background(), "healthy", 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoActiveProvider)
}
