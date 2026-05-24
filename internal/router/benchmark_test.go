package router

import (
	"context"
	"fmt"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

// --- Benchmarks for Resolver.Resolve ---

// buildResolver creates a Resolver with the given number of provider-model mappings.
// All providers are healthy, latency=50ms, active=0.
func buildResolver(numProviders int) *Resolver {
	reg := provider.NewRegistry()
	models := make([]model.ProviderModel, numProviders)
	for i := 0; i < numProviders; i++ {
		name := fmt.Sprintf("provider-%d", i)
		reg.Register(name, &mockProvider{name: name})
		models[i] = model.ProviderModel{
			ID:              int64(i + 1),
			ModelName:       "gpt-4",
			ProviderModel:   fmt.Sprintf("gpt-4-%d", i),
			Weight:          10,
			Priority:        1,
			Status:          1,
			InputPrice:      0.03,
			OutputPrice:     0.06,
			Currency:        "USD",
			RoutingStrategy: "weighted_random",
			Provider:        model.Provider{Name: name, Status: 1},
		}
	}

	repo := &mockProviderModelRepo{
		data: map[string][]model.ProviderModel{
			"gpt-4": models,
		},
	}

	return NewResolver(reg, repo, nil, map[StrategyName]RoutingStrategy{
		StrategyWeightedRandom: &WeightedRandomStrategy{},
	}, &mockLatencySvc{}, &mockActiveTracker{}, nil)
}

type mockLatencySvc struct{}

func (m *mockLatencySvc) GetAvgLatency(_ context.Context, _ string) float64 { return 50.0 }

type mockActiveTracker struct{}

func (m *mockActiveTracker) Get(_ context.Context, _ string) int64 { return 0 }

// BenchmarkResolver_Resolve_CacheMiss measures the full resolve path:
// DB query (mock) + filtering + concurrent metric gathering + strategy selection + cache store.
func BenchmarkResolver_Resolve_CacheMiss(b *testing.B) {
	r := buildResolver(5)
	ctx := context.Background()

	// Pre-warm once so the registry is ready, then clear cache.
	r.Resolve(ctx, "gpt-4")
	r.Invalidate()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Resolve(ctx, "gpt-4")
		r.Invalidate() // force cache miss each iteration
	}
}

// BenchmarkResolver_Resolve_CacheHit measures the cached path:
// sync.Map lookup + TTL check + health filtering.
func BenchmarkResolver_Resolve_CacheHit(b *testing.B) {
	r := buildResolver(5)
	ctx := context.Background()

	// Warm the cache.
	r.Resolve(ctx, "gpt-4")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Resolve(ctx, "gpt-4")
	}
}

// BenchmarkResolver_Resolve_ManyProviders measures cache-miss resolve with 20 providers.
func BenchmarkResolver_Resolve_ManyProviders(b *testing.B) {
	r := buildResolver(20)
	ctx := context.Background()

	r.Resolve(ctx, "gpt-4")
	r.Invalidate()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Resolve(ctx, "gpt-4")
		r.Invalidate()
	}
}

// BenchmarkStrategy_WeightedRandom measures strategy selection in isolation.
func BenchmarkStrategy_WeightedRandom(b *testing.B) {
	s := &WeightedRandomStrategy{}
	candidates := make([]RouteCandidate, 10)
	for i := range candidates {
		name := fmt.Sprintf("provider-%d", i)
		candidates[i] = RouteCandidate{
			Provider:      &mockProvider{name: name},
			ProviderModel: fmt.Sprintf("model-%d", i),
			ProviderRow:   &model.Provider{Name: name, Status: 1},
			Weight:        10,
			Priority:      1,
			ModelID:       int64(i + 1),
			ModelName:     "gpt-4",
		}
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Select(ctx, candidates)
	}
}
