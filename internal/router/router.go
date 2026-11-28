package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/secret"
)

// ErrNoActiveProvider is returned by Resolve when the model EXISTS with
// enabled mappings, but every candidate was filtered out at request time —
// typically because all of its providers' circuits are open, or tier/registry
// gating removed them. This is a TEMPORARY server-side condition: clients
// should retry (HTTP 503), not treat the model as unknown (404, reserved for
// the plain "no provider found" case).
var ErrNoActiveProvider = errors.New("no active provider")

// ErrResolveDegraded is returned when the mapping lookup itself failed (DB
// unreachable / pool exhausted): the model is NOT unknown — the gateway
// cannot answer right now. Maps to 503, never 404.
var ErrResolveDegraded = errors.New("resolve degraded")

// ErrResolveContention is returned when the model-mapping generation kept
// changing during a bounded resolve retry: the resolver cannot pin a result
// to a generation and refuses to cache or serve one (M1). Temporary by
// nature — clients should retry, like ErrResolveDegraded.
var ErrResolveContention = errors.New("resolve contention")

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
	// routeMetricsCache: per-provider short TTL (2s) cache of the latency /
	// active-request snapshots the cache-hit path hands to the strategies
	// (least_latency / least_busy read them), so re-selecting per request
	// costs at most one metrics burst per 2s per provider.
	routeMetricsCache sync.Map // key providerName -> *routeMetrics
	// resolveLookupTimeout: r78 availability bound on the resolveUncached DB
	// round trip (nanoseconds; atomic so SetResolveTimeout needs no lock on
	// the read path). Set via SetResolveTimeout — NewResolver keeps its
	// positional signature (overlay capability_test.go calls it positionally).
	resolveTimeout atomic.Int64
	// healthScoreCache: per-(provider,model) short TTL (2s) cache of health scores
	// so the cache-hit hot path doesn't do N Redis reads per request. Bounded
	// staleness (≤2s) is far better than the 30s resolver cache alone.
	healthScoreCache sync.Map // key "pn|pm" -> *healthScoreCacheEntry
	// degraded: BOUNDED negative cache for ErrResolveDegraded per (model,org).
	// Without it, a DB outage turns EVERY request into a fresh mapping query
	// through a pool that cannot dial — the query failure storm observed in
	// the ladder campaigns (cap3: 345k 503s, each one a DB attempt plus a
	// usage write). One failure suppresses further lookups for a short window.
	// Bounded (degradedCacheCap) with lazy expiry on read AND insert-time
	// sweeping — unbounded (model,org) churn must not accumulate.
	degradedMu sync.Mutex
	degraded   map[string]time.Time // cacheKey -> suppressed-until
	// probes: per-key single-flight for resolveUncached. Cold start or the
	// instant a degradation window expires would otherwise let N concurrent
	// requests query the DB together; one probe runs, the rest share its
	// outcome.
	probesMu sync.Mutex
	probes   map[string]*resolveProbe
	// M1 write-invalidation barrier: epoch increments on every invalidation;
	// cache publishes and degraded-negative-cache writes are guarded under
	// invalidationMu against the epoch the query started on, so an in-flight
	// query that read the DB before an invalidation can neither backfill
	// stale mappings nor suppress the next generation's lookups (the race the
	// unconditional cache.Store used to lose).
	invalidationMu sync.Mutex
	epoch          atomic.Uint64
}

// resolveDegradedTTL bounds how long a resolve-degradation negative-cache
// entry suppresses DB lookups. Short on purpose: after the DB recovers,
// traffic sees at most this much extra 503 latency.
var resolveDegradedTTL = 3 * time.Second

// degradedCacheCap bounds the negative cache. Keys are "model:orgID"; 8192
// entries is far beyond any real model/org fan-out and costs < 1MB.
var degradedCacheCap = 8192

// resolveProbe is one in-flight resolveUncached for a key; late arrivals
// wait on done and reuse result/err verbatim. cancelled marks an outcome
// produced by the OWNER's request cancellation — it carries no verdict about
// DB health and must not be cached or treated as degradation.
type resolveProbe struct {
	done      chan struct{}
	result    []*RouteResult
	err       error
	cancelled bool
}

// cacheEntry is one resolved mapping set for a (model, org). It holds the
// PRE-selection pool: the routing strategy is run for every request, cache
// hits included (serveFromCache). Caching a post-selection order would instead
// replay one request's pick for the whole TTL — every strategy degenerated
// into "first route of the cached order" for all but the cache-filling request.
type cacheEntry struct {
	candidates   []RouteCandidate
	strategyName StrategyName
	expire       time.Time
}

// routeMetrics is a cached per-provider latency / active-request snapshot.
type routeMetrics struct {
	latency float64
	active  int64
	expire  time.Time
}

const routeMetricsTTL = 2 * time.Second

// healthScoreCacheEntry is a cached HealthScore result with a short TTL.
type healthScoreCacheEntry struct {
	score  float64
	expire time.Time
}

// SetHealthScoreFn injects the health-score closure used to demote unhealthy
// routes on cache hit (P4.4b). nil disables health-aware demotion; selection
// falls back to the binary IsHealthyModel filter (pre-P4 behavior).
func (r *Resolver) SetHealthScoreFn(fn HealthScoreFn) { r.healthFn = fn }

// CachedHealthScoreFn returns a health-score closure backed by the resolver's
// 2s per-(provider, model) cache, or nil when no scorer is wired. Strategies
// run on every request (cache hits included), so they must be handed THIS
// closure rather than the raw scorer: their filterByHealth would otherwise
// cost up to three Redis reads per primary per request.
func (r *Resolver) CachedHealthScoreFn() HealthScoreFn {
	if r.healthFn == nil {
		return nil
	}
	return r.cachedHealthScore
}

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
	// Modality (图片方案 P3, design §5.2): the endpoint family this mapping
	// declares — "" = unclassified (kept everywhere; zero-break legacy), an
	// explicit value is a hard gate (see FilterRoutesByModality).
	Modality string
	// M3.2/P3: model-level extension controls parsed from the mapping's
	// extra_config (validated at write time by ValidateModelExtensionConfig).
	// RequestOverrides: admin injection/override of allowed generation params
	// (wins over client-sent values per the M3.2 priority order).
	// AllowedExtensions: dialect params this route opts in to forwarding.
	RequestOverrides  map[string]any
	AllowedExtensions []string
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
	r := &Resolver{
		registry:       registry,
		repo:           repo,
		aliasResolver:  aliasResolver,
		health:         health,
		ttl:            30 * time.Second,
		strategies:     strategies,
		latencySvc:     latencySvc,
		activeTracker:  activeTracker,
		secretResolver: secretResolver,
		degraded:       make(map[string]time.Time),
		probes:         make(map[string]*resolveProbe),
	}
	r.resolveTimeout.Store(int64(2 * time.Second))
	return r
}

// SetResolveTimeout bounds the resolveUncached DB round trip (r78
// availability fix: reach the degraded negative-cache / typed-503 path fast
// instead of holding the single-flight probe for the driver dial timeout).
// Wiring: gateway.resolve_lookup_timeout.
func (r *Resolver) SetResolveTimeout(d time.Duration) {
	if d > 0 {
		r.resolveTimeout.Store(int64(d))
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

	cacheKey := fmt.Sprintf("%s:%d", modelName, orgID)
	// Degradation negative-cache: while a recent mapping-lookup failure
	// suppresses DB access, answer 503 from memory instead of re-dialing a
	// failing pool per request.
	if err := r.degradedError(cacheKey, modelName); err != nil {
		return nil, err
	}
	if routes, err, hit := r.serveFromCache(ctx, cacheKey, modelName); hit {
		return routes, err
	}

	// M1: the probe key carries the mapping generation (epoch) captured at
	// query start — requests arriving after an invalidation must not join a
	// probe whose outcome may predate the change; they start a fresh probe
	// against the new generation.
	e := r.epoch.Load()
	probeKey := fmt.Sprintf("%s#%d", cacheKey, e)

	// Single-flight the uncached probe per key: cold start and the instant a
	// degradation window expires would otherwise stampede the DB with N
	// concurrent identical queries. The probe owner runs resolveUncached and
	// publishes; everyone else waits and reuses the outcome verbatim.
	//
	// The caching/degradation policy runs INSIDE the probe, before the
	// outcome is published (close(done)) and before the probe unregisters —
	// so a post-completion arrival sees the caches already filled and never
	// starts a second query (the tail race the owner-side Store used to lose).
	return r.probeResolve(ctx, probeKey, func(pctx context.Context) ([]*RouteResult, error) {
		// Tail-race guard: an earlier probe may have completed (and filled
		// either cache) between this caller's checks and its probe
		// registration. The new owner re-checks both before touching the DB
		// so the stampede stays collapsed at one real query.
		if derr := r.degradedError(cacheKey, modelName); derr != nil {
			return nil, derr
		}
		if routes, cerr, hit := r.serveFromCache(ctx, cacheKey, modelName); hit {
			return routes, cerr
		}
		// M1 bounded re-resolve loop: when an invalidation races this query,
		// retry once on the new generation; continuous churn returns a typed
		// contention error instead of looping, caching stale routes, or
		// serving a result from an unpinnable generation.
		for attempt := 0; ; attempt++ {
			plan, err := r.resolvePlan(pctx, modelName, orgID)
			if err != nil {
				// Negative-cache ONLY the degradation case (the DB lookup
				// itself failed on the DB's merits), and only on the
				// generation the query started on — an old query's failure
				// must not suppress the next generation's lookups. Cancellations
				// come back as raw ctx errors from resolvePlan and must
				// NOT poison the negative cache.
				if errors.Is(err, ErrResolveDegraded) {
					r.markDegradedIfEpoch(cacheKey, e)
				}
				return nil, err
			}
			r.invalidationMu.Lock()
			cur := r.epoch.Load()
			if cur == e {
				r.cache.Store(cacheKey, &cacheEntry{
					candidates:   plan.candidates,
					strategyName: plan.strategyName,
					expire:       time.Now().Add(r.ttl),
				})
				r.invalidationMu.Unlock()
				r.clearDegraded(cacheKey)
				return plan.selectRoutes(pctx, r), nil
			}
			r.invalidationMu.Unlock()
			if attempt >= 1 {
				return nil, fmt.Errorf("%w: model %s mapping changed repeatedly during resolve", ErrResolveContention, modelName)
			}
			e = cur // re-resolve once against the new generation
		}
	})
}

// markDegradedIfEpoch records the degradation negative-cache entry only when
// the mapping generation is unchanged since the query started (M1).
func (r *Resolver) markDegradedIfEpoch(cacheKey string, e uint64) {
	r.invalidationMu.Lock()
	defer r.invalidationMu.Unlock()
	if r.epoch.Load() != e {
		return
	}
	r.markDegraded(cacheKey)
}

// serveFromCache answers from a fresh cache entry, or reports hit=false when
// none exists. Extracted from the Resolve fast path so the single-flight
// probe owner can re-check it as a tail-race guard.
//
// The entry holds the pre-selection candidate pool, so the routing strategy
// runs again for THIS request (see cacheEntry) instead of replaying the order
// chosen when the entry was filled.
func (r *Resolver) serveFromCache(ctx context.Context, cacheKey, modelName string) ([]*RouteResult, error, bool) {
	v, ok := r.cache.Load(cacheKey)
	if !ok {
		return nil, nil, false
	}
	entry := v.(*cacheEntry)
	if !time.Now().Before(entry.expire) {
		r.cache.Delete(cacheKey)
		return nil, nil, false
	}
	candidates := entry.candidates
	// Filter out circuit-open providers via a PURE READ (healthAllows →
	// CircuitState). Resolve must NOT use IsHealthyModel here: it claims the
	// half-open probe lease (probeInFlight), and the FallbackEngine re-checks
	// IsHealthyModel before each attempt — it would see the lease Resolve
	// just claimed, skip the provider, and never run the attempt whose
	// deferred ClearProbe releases it. The leaked lease then permanently
	// filters the provider out of Resolve ("no available route for model"
	// deadlock).
	if r.health != nil {
		healthy := make([]RouteCandidate, 0, len(candidates))
		for _, c := range candidates {
			if r.healthAllows(c.Provider.Name(), c.ProviderModel) {
				healthy = append(healthy, c)
			}
		}
		if len(healthy) == 0 {
			// All cached providers circuit-open: serve the REJECTION from
			// the cached mappings instead of falling through to the DB.
			// The previous fallthrough re-queried provider_models for EVERY
			// rejected request — an upstream outage under load became a DB
			// query storm (measured in soak r2: 178k slow provider_models
			// queries at ~200ms each saturated Postgres, exhausting its
			// connections and cascading into resolve-degraded errors).
			// Circuit state is still checked live per request (pure read
			// above); mapping edits invalidate this cache explicitly.
			return nil, r.unroutableFromCache(modelName, candidatesToRouteResults(candidates)), true
		}
		candidates = healthy
	}
	if len(candidates) == 0 {
		// Unreachable for entries Resolve stores (resolvePlan returns at
		// least one candidate); an empty entry must never answer as a hit.
		return nil, nil, false
	}
	return r.applyStrategy(ctx, r.prepareCandidates(ctx, candidates), entry.strategyName), nil, true
}

// probeResolve runs fn for cacheKey at most once among concurrent callers.
// Cancellation semantics (review directive): every caller brings its own ctx —
//   - a WAITER whose ctx is cancelled returns immediately instead of pinning
//     the request on someone else's probe;
//   - an OWNER whose ctx is cancelled mid-probe publishes a cancelled
//     (non-verdict) outcome and unregisters; live waiters retry once rather
//     than inheriting a foreign cancellation, and nothing about the DB is
//     cached or marked degraded from a cancellation.
func (r *Resolver) probeResolve(ctx context.Context, cacheKey string, fn func(context.Context) ([]*RouteResult, error)) ([]*RouteResult, error) {
	for attempt := 0; ; attempt++ {
		r.probesMu.Lock()
		if p, ok := r.probes[cacheKey]; ok {
			r.probesMu.Unlock()
			select {
			case <-p.done:
				if p.cancelled && attempt == 0 && ctx.Err() == nil {
					// Owner was cancelled — DB verdict unknown. Become the
					// next owner once instead of propagating the foreign
					// cancellation (bounded retry: a second cancellation
					// falls through to the caller).
					continue
				}
				return p.result, p.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		p := &resolveProbe{done: make(chan struct{})}
		r.probes[cacheKey] = p
		r.probesMu.Unlock()

		result, err := fn(ctx)
		if ctx.Err() != nil && err != nil {
			// Owner cancelled with no usable outcome: publish a cancelled
			// (non-verdict) outcome so current waiters return promptly; live
			// waiters/next arrivals re-probe instead of inheriting it. A
			// SUCCESSFUL result stands even when the ctx fired late — it is
			// a real verdict (this keeps e.g. pre-cancelled playground
			// requests whose repo ignores ctx recording their usage).
			//
			// Ordering (review fix): UNREGISTER BEFORE PUBLISHING. With
			// close(done) first, a woken waiter could loop, re-read the SAME
			// still-registered cancelled probe, burn its single retry on it,
			// and return the owner's ctx error. With the registration gone
			// before done closes, a retrying waiter cannot find this probe
			// again: it becomes the next owner and probes with its own ctx.
			p.cancelled = true
			p.result, p.err = nil, err
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				p.err = fmt.Errorf("%w: %v", context.Canceled, err)
			}
			r.forgetProbe(cacheKey, p)
			close(p.done)
			return nil, p.err
		}
		p.result, p.err = result, err
		// Same unregister-before-publish ordering as the cancel branch: the
		// registration is gone before waiters can wake, so a post-completion
		// arrival always observes the caches (filled above by fn's policy)
		// instead of finding a finished-but-registered probe.
		r.forgetProbe(cacheKey, p)
		close(p.done)
		return result, err
	}
}

func (r *Resolver) forgetProbe(cacheKey string, p *resolveProbe) {
	r.probesMu.Lock()
	if cur, ok := r.probes[cacheKey]; ok && cur == p {
		delete(r.probes, cacheKey)
	}
	r.probesMu.Unlock()
}

// markDegraded suppresses lookups for cacheKey for one TTL window. The map is
// bounded: at cap, expired entries are swept first, then arbitrary entries
// are evicted — unbounded (model,org) churn must not accumulate memory.
func (r *Resolver) markDegraded(cacheKey string) {
	r.degradedMu.Lock()
	defer r.degradedMu.Unlock()
	if len(r.degraded) >= degradedCacheCap {
		now := time.Now()
		for k, until := range r.degraded {
			if now.After(until) {
				delete(r.degraded, k)
			}
		}
		for k := range r.degraded {
			if len(r.degraded) < degradedCacheCap {
				break
			}
			delete(r.degraded, k)
		}
	}
	r.degraded[cacheKey] = time.Now().Add(resolveDegradedTTL)
}

func (r *Resolver) clearDegraded(cacheKey string) {
	r.degradedMu.Lock()
	delete(r.degraded, cacheKey)
	r.degradedMu.Unlock()
}

// degradedError returns the cached degradation error for cacheKey when a
// recent failure suppressed lookups, or nil when lookups should proceed.
// Expired entries are removed lazily on read.
func (r *Resolver) degradedError(cacheKey, modelName string) error {
	now := time.Now()
	r.degradedMu.Lock()
	until, ok := r.degraded[cacheKey]
	if ok {
		if now.After(until) {
			delete(r.degraded, cacheKey)
			ok = false
		}
	}
	r.degradedMu.Unlock()
	if !ok {
		return nil
	}
	return fmt.Errorf("%w: query model mappings for model %s: recently failed (suppressed %s)", ErrResolveDegraded, modelName, time.Until(until).Round(time.Millisecond))
}

// resolveUncached performs the DB query, candidate filtering, strategy selection,
// and returns the ordered routes. It does NOT touch the cache — the caller owns
// caching (Resolve stores the plan; the alias resolver bypasses).
func (r *Resolver) resolveUncached(ctx context.Context, modelName string, orgID int64) ([]*RouteResult, error) {
	plan, err := r.resolvePlan(ctx, modelName, orgID)
	if err != nil {
		return nil, err
	}
	return plan.selectRoutes(ctx, r), nil
}

// routePlan is one resolved mapping set: the route candidates (secrets
// resolved) and the strategy that orders them. Resolve caches the plan so a
// cache hit can run selectRoutes for its own request.
type routePlan struct {
	candidates   []RouteCandidate
	strategyName StrategyName
}

func (p *routePlan) selectRoutes(ctx context.Context, r *Resolver) []*RouteResult {
	return r.applyStrategy(ctx, p.candidates, p.strategyName)
}

// resolvePlan is resolveUncached without the selection step.
func (r *Resolver) resolvePlan(ctx context.Context, modelName string, orgID int64) (*routePlan, error) {
	// r78 availability bound: an unreachable DB previously held this probe
	// (and every request waiting on the single-flight) for the full driver
	// dial timeout before the degraded negative-cache kicked in. Bounding the
	// lookup reaches the SAME degraded/typed-503 path fast. Caller
	// cancellation is still distinguished from OUR deadline below.
	lt := r.resolveTimeout.Load()
	if lt <= 0 {
		lt = int64(2 * time.Second)
	}
	lctx, cancel := context.WithTimeout(ctx, time.Duration(lt))
	models, err := r.repo.FindByModelName(lctx, modelName, orgID)
	cancel()
	if err != nil {
		// A cancelled caller says NOTHING about DB health — return the raw
		// cancellation so callers neither negative-cache nor report this as
		// a degradation (a healthy DB must not be suppressed because one
		// request went away mid-query). Our OWN lookup deadline, however, is
		// DB-health evidence exactly like a query error.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: query model mappings for model %s (bounded %s): %v", ErrResolveDegraded, modelName, time.Duration(lt), err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no provider found for model: %s", modelName)
	}

	// Filter active models with registered providers
	var candidates []model.ProviderModel
	var openCircuits []string
	for _, m := range models {
		if m.Status != 1 || m.Provider.Status != 1 {
			continue
		}
		if _, ok := r.registry.Get(m.Provider.Name); !ok {
			continue
		}
		if !r.healthAllows(m.Provider.Name, m.ProviderModel) {
			if d := r.health.OpenCircuitDescription(m.Provider.Name, m.ProviderModel); d != "" {
				openCircuits = append(openCircuits, d)
			}
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
		if len(openCircuits) > 0 {
			// Carry the upstream cause (error type + message recorded when the
			// circuit opened) into logs and the client-facing rejection.
			return nil, fmt.Errorf("%w for model: %s (%s)", ErrNoActiveProvider, modelName, strings.Join(openCircuits, "; "))
		}
		return nil, fmt.Errorf("%w for model: %s", ErrNoActiveProvider, modelName)
	}

	// Determine strategy from first candidate. One that is not registered
	// (e.g. an overlay strategy under a community build) falls back to
	// weighted_random at selection time (applyStrategy).
	strategyName := StrategyName(candidates[0].RoutingStrategy)
	if strategyName == "" {
		strategyName = StrategyWeightedRandom
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
			Weight:         m.Weight,
			Priority:       m.Priority,
			MaxContext:     m.MaxContext,
			Modality:       m.Modality,
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

			RequestOverrides:  ec.RequestOverrides,
			AllowedExtensions: ec.AllowedExtensions,
		})
	}

	if len(routeCandidates) == 0 {
		return nil, fmt.Errorf("%w for model: %s", ErrNoActiveProvider, modelName)
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

	return &routePlan{candidates: routeCandidates, strategyName: strategyName}, nil
}

// healthAllows is Resolve's health filter: a pure CircuitState read that only
// excludes still-OPEN circuits. Half-open (expired) circuits pass so the
// FallbackEngine can claim the probe lease (single-flight, C2) and release it
// via the deferred ClearProbe in attemptNonStream/attemptStream. Resolve never
// claims the lease itself — a lease claimed here is never released (the engine
// would see it and skip the provider), deadlocking the model's routes.
// unroutableFromCache builds the ErrNoActiveProvider rejection for a model
// whose cached mappings are all circuit-open, carrying the recorded upstream
// cause (in-memory health read — no DB access).
func (r *Resolver) unroutableFromCache(modelName string, results []*RouteResult) error {
	var open []string
	if r.health != nil {
		for _, rr := range results {
			if d := r.health.OpenCircuitDescription(rr.Provider.Name(), rr.ProviderModel); d != "" {
				open = append(open, d)
			}
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("%w for model: %s (%s)", ErrNoActiveProvider, modelName, strings.Join(open, "; "))
	}
	return fmt.Errorf("%w for model: %s", ErrNoActiveProvider, modelName)
}

func (r *Resolver) healthAllows(name, model string) bool {
	if r.health == nil {
		return true
	}
	return r.health.CircuitState(name, model) != provider.CircuitOpen
}

// Invalidate removes cached entries. Call this when model mappings change.
// M1: the epoch is bumped under the same lock that deletes entries, closing
// the in-flight backfill race — a resolve that queried the DB before this
// invalidation can no longer Store its (now stale) result afterwards.
func (r *Resolver) Invalidate() {
	r.invalidateWhere(func(string) bool { return true })
}

// InvalidateModel removes the cached entries of one model name across all
// orgs — the targeted form used by the M1 cross-instance invalidate_model
// notification. Cache keys are "model:orgID"; the model name is matched by
// parsing at the LAST colon so model names that themselves contain ':' cannot
// interfere with each other (no bare prefix matching).
func (r *Resolver) InvalidateModel(name string) {
	r.invalidateWhere(func(k string) bool {
		i := strings.LastIndexByte(k, ':')
		return i >= 0 && k[:i] == name
	})
}

func (r *Resolver) invalidateWhere(match func(string) bool) {
	r.invalidationMu.Lock()
	r.epoch.Add(1)
	r.cache.Range(func(key, _ any) bool {
		if s, ok := key.(string); ok && match(s) {
			r.cache.Delete(key)
		}
		return true
	})
	r.invalidationMu.Unlock()
}

// prepareCandidates readies cached candidates for one selection: it copies
// them (the cached pool is shared by every request and never mutated),
// refreshes the latency / active-request snapshots the latency- and
// load-aware strategies read, and demotes health=0 primaries.
func (r *Resolver) prepareCandidates(ctx context.Context, candidates []RouteCandidate) []RouteCandidate {
	out := make([]RouteCandidate, len(candidates))
	copy(out, candidates)
	r.refreshRouteMetrics(ctx, out)
	r.demoteOnHealth(out)
	return out
}

// refreshRouteMetrics updates each candidate's latency / active-request
// snapshot in place. Both are Redis reads per provider, so they go through a
// 2s per-provider cache: re-selecting on every cache hit costs at most one
// burst per 2s per provider instead of two reads per candidate per request.
// A single candidate has nothing to choose between and skips the reads.
func (r *Resolver) refreshRouteMetrics(ctx context.Context, candidates []RouteCandidate) {
	if (r.latencySvc == nil && r.activeTracker == nil) || len(candidates) < 2 {
		return
	}
	now := time.Now()
	// provider name -> candidate indexes waiting on a fresh snapshot
	pending := make(map[string][]int)
	for i, c := range candidates {
		if v, ok := r.routeMetricsCache.Load(c.Provider.Name()); ok {
			if m, ok := v.(*routeMetrics); ok && now.Before(m.expire) {
				candidates[i].AvgLatencyMs, candidates[i].ActiveRequests = m.latency, m.active
				continue
			}
		}
		pending[c.Provider.Name()] = append(pending[c.Provider.Name()], i)
	}
	if len(pending) == 0 {
		return
	}
	names := make([]string, 0, len(pending))
	for name := range pending {
		names = append(names, name)
	}
	fetched := make([]routeMetrics, len(names))
	var wg sync.WaitGroup
	for k, name := range names {
		wg.Add(1)
		go func(k int, name string) {
			defer wg.Done()
			if r.latencySvc != nil {
				fetched[k].latency = r.latencySvc.GetAvgLatency(ctx, name)
			}
			if r.activeTracker != nil {
				fetched[k].active = r.activeTracker.Get(ctx, name)
			}
		}(k, name)
	}
	wg.Wait()
	expire := time.Now().Add(routeMetricsTTL)
	for k, name := range names {
		m := fetched[k]
		m.expire = expire
		r.routeMetricsCache.Store(name, &m)
		for _, i := range pending[name] {
			candidates[i].AvgLatencyMs, candidates[i].ActiveRequests = m.latency, m.active
		}
	}
}

// demoteOnHealth moves primaries whose cached health score is 0 into the
// fallback pool for this selection only. Doing it here keeps the P4.4b
// guarantee — a demoted route is never picked while a healthy one exists —
// independent of whether the strategy was wired with a health scorer.
func (r *Resolver) demoteOnHealth(candidates []RouteCandidate) {
	if r.healthFn == nil || len(candidates) < 2 {
		return
	}
	for i := range candidates {
		if candidates[i].Weight > 0 && r.cachedHealthScore(candidates[i].Provider.Name(), candidates[i].ProviderModel) <= 0 {
			candidates[i].Weight = 0
		}
	}
}

// applyStrategy orders candidates for ONE request: the strategy picks the
// first route and arranges the rest (the remaining primaries, then the
// fallback pool) — the same shape the uncached path has always produced. An
// unregistered strategy (e.g. an overlay strategy under a community build)
// falls back to weighted_random; with no strategy at all the candidates are
// only split into that shape.
func (r *Resolver) applyStrategy(ctx context.Context, candidates []RouteCandidate, name StrategyName) []*RouteResult {
	strategy := r.strategies[name]
	if strategy == nil {
		strategy = r.strategies[StrategyWeightedRandom]
	}
	if strategy != nil {
		_, ordered := strategy.Select(ctx, candidates)
		return ordered
	}
	primaries, fallbacks := splitPrimariesFallbacks(candidates)
	return append(candidatesToRouteResults(primaries), candidatesToRouteResults(fallbacks)...)
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

// Registry exposes the provider registry (used by background services that
// need to look providers up by name, e.g. batch completion billing).
func (r *Resolver) Registry() *provider.Registry { return r.registry }

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
