package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// End-to-end circuit lifecycle tests: the exact production call sequence used
// by the playground and /v1 handlers (playground.go Chat:
// Resolver.Resolve → ExpandFallbackRoutes → NewFallbackEngine → Execute),
// driven against a scriptable upstream. They verify that the resolver's pure
// CircuitState filter keeps the breaker semantics (open = excluded, no
// upstream traffic) while half-open probes flow through the FallbackEngine,
// where the probe lease is claimed and released by the attempt itself.

// scriptedProvider returns queued Chat outcomes (true = success, false = 500
// server error); once the queue is empty every call succeeds. If block is
// non-nil, calls park on it first (used to hold a probe in flight).
type scriptedProvider struct {
	name     string
	mu       sync.Mutex
	outcomes []bool
	calls    int
	block    chan struct{}
}

func (p *scriptedProvider) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	p.mu.Lock()
	p.calls++
	ok := true
	if len(p.outcomes) > 0 {
		ok = p.outcomes[0]
		p.outcomes = p.outcomes[1:]
	}
	block := p.block
	p.mu.Unlock()
	if block != nil {
		<-block
	}
	if ok {
		return &domain.OpenAIResponse{Model: p.name}, nil
	}
	return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "boom"}
}

func (p *scriptedProvider) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	ch := make(chan domain.SSEChunk, 1)
	ch <- domain.SSEChunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type lifecycleRepo struct {
	data map[string][]model.ProviderModel
}

func (m *lifecycleRepo) FindByModelName(_ context.Context, name string, _ int64) ([]model.ProviderModel, error) {
	return m.data[name], nil
}

// lifecycleRequest mirrors playground.go Chat end to end: resolve, expand
// fallbacks, execute with fallback. A Resolve error here is what surfaces as
// the production "no available route for model" 404.
func lifecycleRequest(ctx context.Context, resolver *router.Resolver, health *provider.HealthTracker) (*FallbackResult, error) {
	routes, err := resolver.Resolve(ctx, "MiniMax-M3", 0)
	if err != nil {
		return nil, err
	}
	routes = ExpandFallbackRoutes(ctx, resolver, routes, 0)
	engine := NewFallbackEngine(health, ResolveFallbackConfig(routes))
	return engine.ExecuteNonStream(ctx, routes,
		func(ctx context.Context, route *router.RouteResult) (any, error) {
			return route.Provider.Chat(ctx, &domain.OpenAIRequest{Model: route.ProviderModel}, "")
		}), nil
}

func newLifecycleResolver(health *provider.HealthTracker, reg *provider.Registry, prov *scriptedProvider) *router.Resolver {
	return router.NewResolver(reg, &lifecycleRepo{data: map[string][]model.ProviderModel{
		"MiniMax-M3": {{
			ID: 1, ProviderModel: "MiniMax-M3", Weight: 1, Status: 1,
			Provider: model.Provider{Name: prov.Name(), Status: 1},
		}},
	}}, health, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
}

// Full lifecycle: healthy → 3 failures open the circuit → open window rejects
// at Resolve with zero upstream calls → cooldown expires → half-open probe
// succeeds → circuit closes → traffic flows again. Steps after the cooldown
// deadlocked permanently before the probe-leak fix.
func TestCircuitLifecycle_EndToEnd(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(3, 30*time.Millisecond)
	prov := &scriptedProvider{name: "minimax", outcomes: []bool{false, false, false}}
	reg := provider.NewRegistry()
	reg.Register("minimax", prov)
	resolver := newLifecycleResolver(health, reg, prov)
	ctx := context.Background()

	// Phase 1: three failing requests — each reaches the upstream, each records
	// a transient failure. The third crosses the threshold and opens the circuit.
	for i := 0; i < 3; i++ {
		res, err := lifecycleRequest(ctx, resolver, health)
		if err != nil {
			t.Fatalf("request %d: unexpected resolve error: %v", i+1, err)
		}
		if res.FinalError == nil {
			t.Fatalf("request %d: expected upstream failure, got success", i+1)
		}
	}
	if st := health.CircuitState("minimax", "MiniMax-M3"); st != provider.CircuitOpen {
		t.Fatalf("expected circuit open after 3 failures, got %v", st)
	}

	// Phase 2 (open window): Resolve must reject with the production
	// "no active provider found" error, and the upstream must NOT be called.
	_, err := lifecycleRequest(ctx, resolver, health)
	if err == nil || err.Error() != "no active provider found for model: MiniMax-M3" {
		t.Fatalf("expected resolve rejection while open, got %v", err)
	}
	if n := prov.callCount(); n != 3 {
		t.Fatalf("open circuit must not reach upstream: got %d calls, want 3", n)
	}

	// Phase 3 (half-open): after cooldown the next request is the probe. It
	// must reach the upstream and succeed — pre-fix, Resolve claimed the probe
	// lease here and the engine skipped the provider forever.
	time.Sleep(40 * time.Millisecond)
	if st := health.CircuitState("minimax", "MiniMax-M3"); st != provider.CircuitHalfOpen {
		t.Fatalf("expected half-open after cooldown, got %v", st)
	}
	res, err := lifecycleRequest(ctx, resolver, health)
	if err != nil {
		t.Fatalf("half-open probe request must resolve, got %v (probe-lease leak?)", err)
	}
	if res.FinalError != nil {
		t.Fatalf("half-open probe must succeed, got %v", res.FinalError)
	}
	if n := prov.callCount(); n != 4 {
		t.Fatalf("expected exactly 1 probe call, got %d total", n)
	}

	// Phase 4 (closed again): subsequent traffic flows without errors.
	if st := health.CircuitState("minimax", "MiniMax-M3"); st != provider.CircuitClosed {
		t.Fatalf("expected circuit closed after successful probe, got %v", st)
	}
	for i := 0; i < 3; i++ {
		res, err := lifecycleRequest(ctx, resolver, health)
		if err != nil {
			t.Fatalf("post-recovery request %d failed to resolve: %v (probe-lease leak?)", i+1, err)
		}
		if res.FinalError != nil {
			t.Fatalf("post-recovery request %d failed: %v", i+1, res.FinalError)
		}
	}
	if n := prov.callCount(); n != 7 {
		t.Fatalf("expected 7 total upstream calls, got %d", n)
	}
}

// Single-flight (C2) must survive the fix: while a half-open probe is in
// flight, concurrent requests are turned away at the FallbackEngine gate —
// exactly one upstream probe, no stampede.
func TestCircuitLifecycle_HalfOpenSingleFlight(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(3, 30*time.Millisecond)
	prov := &scriptedProvider{name: "minimax", outcomes: []bool{false, false, false}}
	reg := provider.NewRegistry()
	reg.Register("minimax", prov)
	resolver := newLifecycleResolver(health, reg, prov)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := lifecycleRequest(ctx, resolver, health); err != nil {
			t.Fatalf("setup request %d: %v", i+1, err)
		}
	}
	time.Sleep(40 * time.Millisecond) // half-open

	// Park the probe on block so the single-flight window stays observable.
	prov.block = make(chan struct{})

	const n = 5
	var returned, succeeded atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := lifecycleRequest(ctx, resolver, health)
			if err == nil && res.FinalError == nil {
				succeeded.Add(1)
			}
			returned.Add(1)
		}()
	}

	// The probe reaches upstream and parks; the other four must be turned away
	// by the engine's probe gate and return WITHOUT calling upstream.
	deadline := time.Now().Add(2 * time.Second)
	for returned.Load() < n-1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := returned.Load(); got != n-1 {
		t.Fatalf("expected %d non-probe requests to return while probe parked, got %d", n-1, got)
	}
	if got := prov.callCount(); got != 4 { // 3 setup failures + 1 parked probe
		t.Fatalf("single-flight violated: %d upstream calls while probe in flight, want 4", got)
	}

	close(prov.block) // release the probe; it succeeds
	wg.Wait()
	if got := succeeded.Load(); got != 1 {
		t.Fatalf("expected exactly 1 success, got %d", got)
	}
	if got := prov.callCount(); got != 4 {
		t.Fatalf("expected no extra upstream calls after release, got %d", got)
	}
}

// A provider with an open circuit must not affect routing to a healthy peer —
// the production multi-provider failover path.
func TestCircuitLifecycle_OpenProviderDoesNotAffectPeers(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(3, time.Minute)
	failing := &scriptedProvider{name: "minimax", outcomes: []bool{false, false, false}}
	healthy := &scriptedProvider{name: "backup"}
	reg := provider.NewRegistry()
	reg.Register("minimax", failing)
	reg.Register("backup", healthy)
	// backup at weight 0 = deterministic fallback slot: every request tries
	// minimax first, then falls back to backup on failure.
	resolver := router.NewResolver(reg, &lifecycleRepo{data: map[string][]model.ProviderModel{
		"MiniMax-M3": {
			{ID: 1, ProviderModel: "MiniMax-M3", Weight: 1, Status: 1,
				Provider: model.Provider{Name: "minimax", Status: 1}},
			{ID: 2, ProviderModel: "MiniMax-M3", Weight: 0, Status: 1,
				Provider: model.Provider{Name: "backup", Status: 1}},
		},
	}}, health, map[router.StrategyName]router.RoutingStrategy{
		router.StrategyWeightedRandom: &router.WeightedRandomStrategy{},
	}, nil, nil, nil, nil)
	ctx := context.Background()

	// Three requests: minimax fails once each (engine falls back to backup),
	// burning its circuit open deterministically.
	for i := 0; i < 3; i++ {
		if _, err := lifecycleRequest(ctx, resolver, health); err != nil {
			t.Fatalf("request during burn-in: %v", err)
		}
	}
	if health.CircuitState("minimax", "MiniMax-M3") != provider.CircuitOpen {
		t.Fatal("minimax circuit should be open after burn-in")
	}

	// With minimax excluded, requests must still succeed via backup.
	for i := 0; i < 5; i++ {
		res, err := lifecycleRequest(ctx, resolver, health)
		if err != nil {
			t.Fatalf("request %d must route to healthy peer, got resolve error: %v", i+1, err)
		}
		if res.FinalError != nil {
			t.Fatalf("request %d must succeed via backup, got %v", i+1, res.FinalError)
		}
		if got := res.Route.Provider.Name(); got != "backup" {
			t.Fatalf("request %d routed to %q, want backup", i+1, got)
		}
	}
	if n := healthy.callCount(); n == 0 {
		t.Fatal("healthy peer should have received traffic")
	}
}
