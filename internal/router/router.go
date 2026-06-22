package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/secret"
)

type Resolver struct {
	registry       *provider.Registry
	repo           ProviderModelRepo
	aliasResolver  AliasResolver
	health         *provider.HealthTracker
	cache          sync.Map
	ttl            time.Duration
	strategies     map[StrategyName]RoutingStrategy
	latencySvc     LatencyProvider
	activeTracker  ActiveRequestsProvider
	secretResolver *secret.SecretResolver
}

type cacheEntry struct {
	results []*RouteResult
	expire  time.Time
}

type ProviderModelRepo interface {
	FindByModelName(ctx context.Context, modelName string, orgID int64) ([]model.ProviderModel, error)
}

type ActiveRequestsProvider interface {
	Get(ctx context.Context, providerName string) int64
}

type RouteResult struct {
	Provider       provider.Provider
	ProviderModel  string
	InputPrice     float64
	OutputPrice    float64
	Currency       string
	ProviderRow    *model.Provider
	RetryConfig    provider.RetryConfig
	FallbackModels []string
	FallbackConfig FallbackConfig
	ExtraConfig    json.RawMessage
}

func NewResolver(
	registry *provider.Registry,
	repo ProviderModelRepo,
	health *provider.HealthTracker,
	strategies map[StrategyName]RoutingStrategy,
	latencySvc LatencyProvider,
	activeTracker ActiveRequestsProvider,
	secretResolver *secret.SecretResolver,
	aliasResolver AliasResolver,
) *Resolver {
	return &Resolver{
		registry:       registry,
		repo:           repo,
		aliasResolver:  aliasResolver,
		health:         health,
		ttl:            30 * time.Second,
		strategies:     strategies,
		latencySvc:     latencySvc,
		activeTracker:  activeTracker,
		secretResolver: secretResolver,
	}
}

func (r *Resolver) Resolve(ctx context.Context, modelName string, orgID int64) ([]*RouteResult, error) {
	// Alias resolution is delegated to the injected AliasResolver (nil in
	// Community). Aliases are resolved per-request (never cached) so member/health
	// changes take effect immediately — see design §4.3.
	if r.aliasResolver != nil {
		if routes, err, isAlias := r.aliasResolver.ResolveAlias(ctx, modelName, orgID); isAlias {
			return routes, err
		}
	}

	// Check cache
	cacheKey := fmt.Sprintf("%s:%d", modelName, orgID)
	if v, ok := r.cache.Load(cacheKey); ok {
		entry := v.(*cacheEntry)
		if time.Now().Before(entry.expire) {
			// Filter out unhealthy providers from cached results
			if r.health != nil {
				var healthy []*RouteResult
				for _, rr := range entry.results {
					if r.health.IsHealthyModel(rr.Provider.Name(), rr.ProviderModel) {
						healthy = append(healthy, rr)
					}
				}
				if len(healthy) > 0 {
					return healthy, nil
				}
				// All cached providers unhealthy — cache miss
			} else {
				return entry.results, nil
			}
		}
		r.cache.Delete(cacheKey)
	}

	ordered, err := r.resolveUncached(ctx, modelName, orgID)
	if err != nil {
		return nil, err
	}

	// Store in cache
	r.cache.Store(cacheKey, &cacheEntry{
		results: ordered,
		expire:  time.Now().Add(r.ttl),
	})

	return ordered, nil
}

// resolveUncached performs the DB query, candidate filtering, strategy selection,
// and returns the ordered routes. It does NOT touch the cache — the caller owns
// caching (Resolve stores; the alias resolver bypasses).
func (r *Resolver) resolveUncached(ctx context.Context, modelName string, orgID int64) ([]*RouteResult, error) {
	models, err := r.repo.FindByModelName(ctx, modelName, orgID)
	if err != nil {
		return nil, fmt.Errorf("query model mappings: %w", err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no provider found for model: %s", modelName)
	}

	// Filter active models with registered providers
	var candidates []model.ProviderModel
	for _, m := range models {
		if m.Status != 1 || m.Provider.Status != 1 {
			continue
		}
		if _, ok := r.registry.Get(m.Provider.Name); !ok {
			continue
		}
		if r.health != nil && !r.health.IsHealthyModel(m.Provider.Name, m.ProviderModel) {
			continue
		}
		if meta := provider.GetAdapterMeta(m.Provider.AdapterType); meta != nil && meta.MinimumTier != "" {
			if !tierSufficient(license.G().CurrentTier(), meta.MinimumTier) {
				continue
			}
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no active provider found for model: %s", modelName)
	}

	// Determine strategy from first candidate
	strategyName := StrategyName(candidates[0].RoutingStrategy)
	if strategyName == "" {
		strategyName = StrategyWeightedRandom
	}
	strategy := r.strategies[strategyName]
	if strategy == nil {
		strategy = r.strategies[StrategyWeightedRandom]
	}

	// Build route candidates with runtime metrics
	type candidateMeta struct {
		latency float64
		active  int64
	}
	meta := make([]candidateMeta, len(candidates))
	var wg sync.WaitGroup
	for i, m := range candidates {
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()
			if r.latencySvc != nil {
				meta[idx].latency = r.latencySvc.GetAvgLatency(ctx, name)
			}
			if r.activeTracker != nil {
				meta[idx].active = r.activeTracker.Get(ctx, name)
			}
		}(i, m.Provider.Name)
	}
	wg.Wait()

	var routeCandidates []RouteCandidate
	for i, m := range candidates {
		p, ok := r.registry.Get(m.Provider.Name)
		if !ok {
			continue
		}
		ec := parseExtraConfig(json.RawMessage(m.ExtraConfig))
		routeCandidates = append(routeCandidates, RouteCandidate{
			Provider:       p,
			ProviderModel:  m.ProviderModel,
			ProviderRow:    &m.Provider,
			InputPrice:     m.InputPrice,
			OutputPrice:    m.OutputPrice,
			Currency:       m.Currency,
			Weight:         m.Weight,
			Priority:       m.Priority,
			ModelID:        m.ID,
			ModelName:      m.ModelName,
			AvgLatencyMs:   meta[i].latency,
			ActiveRequests: meta[i].active,
			CanaryPercent:  ec.CanaryPercent,
			RetryConfig: provider.RetryConfig{
				NumRetries:   ec.NumRetries,
				BackoffType:  ec.BackoffType,
				InitialMs:    ec.InitialMs,
				MaxBackoffMs: ec.MaxBackoffMs,
			},
			FallbackModels: ec.FallbackModels,
			FallbackCfg:    ec.Fallback,
			ExtraConfig:    json.RawMessage(m.ExtraConfig),
		})
	}

	if len(routeCandidates) == 0 {
		return nil, fmt.Errorf("no active provider found for model: %s", modelName)
	}

	// Resolve secrets in route candidates
	if r.secretResolver != nil {
		for i := range routeCandidates {
			rc := &routeCandidates[i]
			if rc.ProviderRow == nil || rc.ProviderRow.APIKey == "" {
				continue
			}
			resolved, err := r.secretResolver.Resolve(ctx, rc.ProviderRow.APIKey)
			if err != nil {
				slog.Error("secret resolution failed, skipping provider", "provider", rc.ProviderRow.Name, "error", err)
				continue
			}
			providerCopy := *rc.ProviderRow
			providerCopy.APIKey = resolved
			rc.ProviderRow = &providerCopy
		}
	}

	// Strategy selection
	_, ordered := strategy.Select(ctx, routeCandidates)

	return ordered, nil
}

// Invalidate removes cached entries. Call this when model mappings change.
func (r *Resolver) Invalidate() {
	r.cache.Range(func(key, _ any) bool {
		r.cache.Delete(key)
		return true
	})
}

func (r *Resolver) Health() *provider.HealthTracker {
	if r == nil {
		return nil
	}
	return r.health
}

// ResolveSingle is a convenience method that returns the first (best) route.
func (r *Resolver) ResolveSingle(ctx context.Context, modelName string, orgID int64) (*RouteResult, error) {
	candidates, err := r.Resolve(ctx, modelName, orgID)
	if err != nil {
		return nil, err
	}
	return candidates[0], nil
}

// tierSufficient returns true if the current tier meets or exceeds the required tier.
func tierSufficient(current, required string) bool {
	tierOrder := map[string]int{
		license.TierCommunity:  0,
		license.TierPro:        1,
		license.TierEnterprise: 2,
	}
	return tierOrder[current] >= tierOrder[required]
}
