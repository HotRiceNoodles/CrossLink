package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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
	// P4 (v5.1 D-1): health-score closure for cache-hit reordering. nil = legacy
	// binary IsHealthyModel filter only. Set via SetHealthScoreFn (Setter, NOT
	// NewResolver — overlay capability_test.go calls NewResolver positionally).
	healthFn HealthScoreFn
	// healthScoreCache: per-(provider,model) short TTL (2s) cache of health scores
	// so the cache-hit hot path doesn't do N Redis reads per request. Bounded
	// staleness (≤2s) is far better than the 30s resolver cache alone.
	healthScoreCache sync.Map // key "pn|pm" -> *healthScoreCacheEntry
}

type cacheEntry struct {
	results []*RouteResult
	expire  time.Time
}

// healthScoreCacheEntry is a cached HealthScore result with a short TTL.
type healthScoreCacheEntry struct {
	score  float64
	expire time.Time
}

// SetHealthScoreFn injects the health-score closure used to reorder cached
// routes on cache hit (P4.4b). nil disables cache-hit reordering; selection
// falls back to the binary IsHealthyModel filter (pre-P4 behavior).
func (r *Resolver) SetHealthScoreFn(fn HealthScoreFn) { r.healthFn = fn }

const healthScoreCacheTTL = 2 * time.Second

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
	// P4: carried from RouteCandidate so the resolver cache-hit path can
	// re-derive effective-weight ordering without re-querying the DB.
	Weight   int
	Priority int
	// context window of the provider model, nil = unknown
	MaxContext *int
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
			// Filter out unhealthy providers from cached results.
			// IsHealthyModel is retained: it drives the half-open probe
			// single-flight (claims one probe per expired circuit). P4.4b
			// ADDS health-score reorder ON TOP, not replacing it.
			if r.health != nil {
				var healthy []*RouteResult
				for _, rr := range entry.results {
					if r.health.IsHealthyModel(rr.Provider.Name(), rr.ProviderModel) {
						healthy = append(healthy, rr)
					}
				}
				if len(healthy) > 0 {
					if r.healthFn != nil {
						healthy = r.reorderByHealth(healthy)
					}
					return healthy, nil
				}
				// All cached providers unhealthy — cache miss
			} else {
				if r.healthFn != nil {
					return r.reorderByHealth(entry.results), nil
				}
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
			MaxContext:     m.MaxContext,
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

// reorderByHealth sorts cached routes by effective weight DESC, Priority ASC
// (P4.4b). Effective weight = Weight × healthScore. Routes with Weight=0
// (configured fallbacks) or healthScore=0 (demoted primaries) sort last — the
// sort naturally demotes them without a separate step. Health scores are read
// through cachedHealthScore (2s local cache) so the cache-hit hot path doesn't
// do N Redis reads per request.
func (r *Resolver) reorderByHealth(routes []*RouteResult) []*RouteResult {
	type item struct {
		rr  *RouteResult
		ew  float64
		pri int
	}
	items := make([]item, len(routes))
	for i, rr := range routes {
		score := r.cachedHealthScore(rr.Provider.Name(), rr.ProviderModel)
		items[i] = item{rr: rr, ew: float64(rr.Weight) * score, pri: rr.Priority}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].ew != items[j].ew {
			return items[i].ew > items[j].ew
		}
		return items[i].pri < items[j].pri
	})
	out := make([]*RouteResult, len(items))
	for i, it := range items {
		out[i] = it.rr
	}
	return out
}

// cachedHealthScore returns the health score for (provider, model) from the
// 2s local cache, computing + storing it on miss. Bounds Redis reads (via
// healthFn) to once per 2s per (provider,model) regardless of request rate.
func (r *Resolver) cachedHealthScore(providerName, model string) float64 {
	key := providerName + "|" + model
	if v, ok := r.healthScoreCache.Load(key); ok {
		if ce, ok := v.(*healthScoreCacheEntry); ok && time.Now().Before(ce.expire) {
			return ce.score
		}
	}
	score := r.healthFn(providerName, model)
	r.healthScoreCache.Store(key, &healthScoreCacheEntry{score: score, expire: time.Now().Add(healthScoreCacheTTL)})
	return score
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
