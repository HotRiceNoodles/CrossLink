package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	if routes, err, hit := r.serveFromCache(cacheKey, modelName); hit {
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
		if routes, cerr, hit := r.serveFromCache(cacheKey, modelName); hit {
			return routes, cerr
		}
		// M1 bounded re-resolve loop: when an invalidation races this query,
		// retry once on the new generation; continuous churn returns a typed
		// contention error instead of looping, caching stale routes, or
		// serving a result from an unpinnable generation.
		for attempt := 0; ; attempt++ {
			ordered, err := r.resolveUncached(pctx, modelName, orgID)
			if err != nil {
				// Negative-cache ONLY the degradation case (the DB lookup
				// itself failed on the DB's merits), and only on the
				// generation the query started on — an old query's failure
				// must not suppress the next generation's lookups. Cancellations
				// come back as raw ctx errors from resolveUncached and must
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
					results: ordered,
					expire:  time.Now().Add(r.ttl),
				})
				r.invalidationMu.Unlock()
				r.clearDegraded(cacheKey)
				return ordered, nil
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

// serveFromCache returns the cached routes for cacheKey with circuit-open
// providers filtered out, or hit=false when no fresh entry exists. Extracted
// from the Resolve fast path so the single-flight probe owner can re-check it
// as a tail-race guard.
func (r *Resolver) serveFromCache(cacheKey, modelName string) ([]*RouteResult, error, bool) {
	v, ok := r.cache.Load(cacheKey)
	if !ok {
		return nil, nil, false
	}
	entry := v.(*cacheEntry)
	if !time.Now().Before(entry.expire) {
		r.cache.Delete(cacheKey)
		return nil, nil, false
	}
	// Filter out circuit-open providers from cached results via a
	// PURE READ (healthAllows → CircuitState). Resolve must NOT use
	// IsHealthyModel here: it claims the half-open probe lease
	// (probeInFlight), and the FallbackEngine re-checks
	// IsHealthyModel before each attempt — it would see the lease
	// Resolve just claimed, skip the provider, and never run the
	// attempt whose deferred ClearProbe releases it. The leaked
	// lease then permanently filters the provider out of Resolve
	// ("no available route for model" deadlock). P4.4b health-score
	// reorder stays ON TOP of this filter.
	if r.health != nil {
		var healthy []*RouteResult
		for _, rr := range entry.results {
			if r.healthAllows(rr.Provider.Name(), rr.ProviderModel) {
				healthy = append(healthy, rr)
			}
		}
		if len(healthy) > 0 {
			if r.healthFn != nil {
				healthy = r.reorderByHealth(healthy)
			}
			return healthy, nil, true
		}
		// All cached providers circuit-open: serve the REJECTION from
		// the cached mappings instead of falling through to the DB.
		// The previous fallthrough re-queried provider_models for EVERY
		// rejected request — an upstream outage under load became a DB
		// query storm (measured in soak r2: 178k slow provider_models
		// queries at ~200ms each saturated Postgres, exhausting its
		// connections and cascading into resolve-degraded errors).
		// Circuit state is still checked live per request (pure read
		// above); mapping edits invalidate this cache explicitly.
		return nil, r.unroutableFromCache(modelName, entry.results), true
	}
	if r.healthFn != nil {
		return r.reorderByHealth(entry.results), nil, true
	}
	return entry.results, nil, true
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
// caching (Resolve stores; the alias resolver bypasses).
func (r *Resolver) resolveUncached(ctx context.Context, modelName string, orgID int64) ([]*RouteResult, error) {
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

	// Strategy selection
	_, ordered := strategy.Select(ctx, routeCandidates)

	return ordered, nil
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
