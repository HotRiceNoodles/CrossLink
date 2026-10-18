package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// probeProvider counts calls and controls the outcome of Chat and the models
// probe. listerErr simulates the /models endpoint outcome (nil = 2xx list);
// lastPath records the path the prober asked for.
type probeProvider struct {
	scriptedProvider
	listerErr error
	modelHits int
	lastPath  string
}

func (p *probeProvider) ListUpstreamModels(ctx context.Context, key string) ([]provider.UpstreamModel, error) {
	return p.ListUpstreamModelsAtPath(ctx, key, "/models")
}

func (p *probeProvider) ListUpstreamModelsAtPath(_ context.Context, _ string, path string) ([]provider.UpstreamModel, error) {
	p.modelHits++
	p.lastPath = path
	if p.listerErr != nil {
		return nil, p.listerErr
	}
	return []provider.UpstreamModel{{ID: "m"}}, nil
}

type probeRows struct {
	row *model.Provider
}

func (r probeRows) GetByName(_ context.Context, _ string) (*model.Provider, error) {
	return r.row, nil
}

type probeModels struct {
	first *model.ProviderModel
}

func (m probeModels) FirstByProviderID(_ context.Context, _ int64) (*model.ProviderModel, error) {
	return m.first, nil
}

func newTestProber(health *provider.HealthTracker, prov *probeProvider) *CircuitProber {
	return newTestProberPath(health, prov, "/models")
}

func newTestProberPath(health *provider.HealthTracker, prov *probeProvider, path string) *CircuitProber {
	reg := provider.NewRegistry()
	reg.Register(prov.Name(), prov)
	row := &model.Provider{ID: 1, Name: prov.Name(), Status: 1, APIKey: "sk-test"}
	return NewCircuitProber(health, reg, probeRows{row}, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second, path)
}

// A successful /models probe closes an open circuit early.
func TestCircuitProber_ModelsProbeClosesCircuit(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}}
	prov.outcomes = []bool{false}
	health.RecordTransientFailure("minimax", "", 0) // open for an hour
	if health.IsHealthy("minimax") {
		t.Fatal("circuit should be open")
	}

	newTestProber(health, prov).ProbeOnce(context.Background())

	if !health.IsHealthy("minimax") {
		t.Fatal("successful /models probe must close the circuit")
	}
	if prov.callCount() != 0 {
		t.Fatalf("/models probe must not spend chat calls, got %d", prov.callCount())
	}
	if prov.modelHits != 1 {
		t.Fatalf("expected exactly 1 models probe, got %d", prov.modelHits)
	}
	if prov.lastPath != "/models" {
		t.Fatalf("default probe path must be /models, got %q", prov.lastPath)
	}
}

// F-12: a NON-2xx /models outcome (endpoint missing / auth-gated /
// rate-limited / 5xx / network error) is inconclusive for model
// serviceability — the prober must fall back to a real 1-token chat, and a
// successful chat closes the circuit. This is the mockgate/r12d failure mode:
// upstream without /models previously NEVER recovered.
func TestCircuitProber_ModelsNon2xxFallsBackToChat(t *testing.T) {
	modelsErrs := map[string]error{
		"404 not implemented": &provider.ProviderError{StatusCode: 404, ErrorType: provider.ErrorNotFound, Message: "404 page not found"},
		"401 auth required":   &provider.ProviderError{StatusCode: 401, ErrorType: provider.ErrorAuth, Message: "unauthorized"},
		"429 rate limited":    &provider.ProviderError{StatusCode: 429, ErrorType: provider.ErrorRateLimit, Message: "rate limited"},
		"500 server error":    &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "boom"},
		"network unreachable": fmt.Errorf("send request: dial tcp 10.0.0.9:443: connectex: timed out"),
	}
	for name, merr := range modelsErrs {
		t.Run(name, func(t *testing.T) {
			health := provider.NewHealthTrackerWithConfig(1, time.Hour)
			prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}, listerErr: merr}
			prov.outcomes = []bool{true} // chat succeeds
			health.RecordTransientFailure("minimax", "", 0)
			if health.IsHealthy("minimax") {
				t.Fatal("circuit should be open")
			}

			newTestProber(health, prov).ProbeOnce(context.Background())

			if !health.IsHealthy("minimax") {
				t.Fatal("failed /models probe must fall back to chat and close on chat success")
			}
			if prov.modelHits != 1 || prov.callCount() != 1 {
				t.Fatalf("expected 1 models probe + 1 chat probe, got models=%d chats=%d", prov.modelHits, prov.callCount())
			}
		})
	}
}

// F-12: when BOTH the models probe and the chat probe fail, the circuit stays
// open and — critically — the prober must never extend openUntil (a failed
// out-of-band probe must not turn the cooldown into an indefinite block).
func TestCircuitProber_FailedProbeLeavesStateUntouched(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"},
		listerErr: context.DeadlineExceeded}
	prov.outcomes = []bool{false, false} // both chat probes fail
	health.RecordTransientFailure("minimax", "", 0)
	var until time.Time
	for _, s := range health.Snapshot() {
		if s.Model == "" {
			until = s.Until
		}
	}

	newTestProber(health, prov).ProbeOnce(context.Background())
	newTestProber(health, prov).ProbeOnce(context.Background())

	if health.IsHealthy("minimax") {
		t.Fatal("failed probes must not close the circuit")
	}
	for _, s := range health.Snapshot() {
		if s.Model == "" && !s.Until.Equal(until) {
			t.Fatalf("openUntil moved after failed probes: %v -> %v", until, s.Until)
		}
	}
	if prov.modelHits != 2 || prov.callCount() != 2 {
		t.Fatalf("expected exactly one models+chat pair per pass, got models=%d chats=%d", prov.modelHits, prov.callCount())
	}
}

// F-12: transient upstream failure then recovery — the first pass sees the
// upstream still failing and stays open; once real traffic would succeed, the
// next pass closes the circuit (bounded: exactly one chat per pass).
func TestCircuitProber_TransientFailureThenRecovers(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"},
		listerErr: &provider.ProviderError{StatusCode: 404, ErrorType: provider.ErrorNotFound, Message: "404"}}
	prov.outcomes = []bool{false} // first chat probe fails too
	health.RecordTransientFailure("minimax", "", 0)

	p := newTestProber(health, prov)
	p.ProbeOnce(context.Background())
	if health.IsHealthy("minimax") {
		t.Fatal("upstream still failing: circuit must stay open")
	}

	p.ProbeOnce(context.Background()) // chat now succeeds (outcomes exhausted)
	if !health.IsHealthy("minimax") {
		t.Fatal("recovered upstream: circuit must close on the next pass")
	}
}

// F-12: the models probe path is configurable; the prober must ask the
// upstream at exactly the configured path.
func TestCircuitProber_CustomProbePath(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}}
	prov.outcomes = []bool{false}
	health.RecordTransientFailure("minimax", "", 0)

	newTestProberPath(health, prov, "/v1/models").ProbeOnce(context.Background())

	if prov.lastPath != "/v1/models" {
		t.Fatalf("probe must use configured path /v1/models, got %q", prov.lastPath)
	}
	if !health.IsHealthy("minimax") {
		t.Fatal("2xx at the configured path must close the circuit")
	}
	if prov.callCount() != 0 {
		t.Fatalf("models 2xx must not spend chat calls, got %d", prov.callCount())
	}
}

// F-12: an empty probe path disables the models probe entirely — decisions
// come from the real chat probe alone (chat-only deployments).
func TestCircuitProber_EmptyPathIsChatOnly(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"},
		listerErr: &provider.ProviderError{StatusCode: 404, ErrorType: provider.ErrorNotFound, Message: "404"}}
	prov.outcomes = []bool{false}
	health.RecordTransientFailure("minimax", "", 0)

	p := newTestProberPath(health, prov, "")
	p.ProbeOnce(context.Background())

	if prov.modelHits != 0 {
		t.Fatalf("empty path must skip the models probe, got %d hits", prov.modelHits)
	}
	if health.IsHealthy("minimax") {
		t.Fatal("chat probe failed: circuit must stay open")
	}
	if prov.callCount() != 1 {
		t.Fatalf("exactly one chat probe expected, got %d", prov.callCount())
	}
}

// F-12 herd bound: one pass probes each open circuit exactly once, however
// many are open (per-circuit-per-interval bound; the half-open single-flight
// gate bounds real traffic on top of this).
func TestCircuitProber_OneProbePerCircuitPerPass(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	names := []string{"pa", "pb", "pc"}
	provs := make([]*probeProvider, len(names))
	reg := provider.NewRegistry()
	for i, n := range names {
		provs[i] = &probeProvider{scriptedProvider: scriptedProvider{name: n}}
		provs[i].outcomes = []bool{false}
		reg.Register(n, provs[i])
		health.RecordTransientFailure(n, "", 0)
	}
	row := &model.Provider{ID: 1, Name: "pa", Status: 1, APIKey: "sk-test"}
	NewCircuitProber(health, reg, probeRows{row}, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second, "/models").
		ProbeOnce(context.Background())

	for i, n := range names {
		if provs[i].modelHits != 1 {
			t.Fatalf("%s: expected exactly 1 models probe, got %d", n, provs[i].modelHits)
		}
		if !health.IsHealthy(n) {
			t.Fatalf("%s: 2xx models must close", n)
		}
	}
}

// Without a /models endpoint, the prober falls back to a 1-token chat on the
// circuit's model and closes on success.
func TestCircuitProber_ChatFallbackClosesCircuit(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	// scriptedProvider has no models methods at all → chat-only path.
	prov := &scriptedProvider{name: "minimax"}
	health.RecordPersistentFailure("minimax", "MiniMax-M3", "model", time.Hour)

	reg := provider.NewRegistry()
	reg.Register("minimax", prov)
	row := &model.Provider{ID: 1, Name: "minimax", Status: 1, APIKey: "sk-test"}
	NewCircuitProber(health, reg, probeRows{row}, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second, "/models").
		ProbeOnce(context.Background())

	if !health.IsHealthyModel("minimax", "MiniMax-M3") {
		t.Fatal("successful chat probe must close the model-scope circuit")
	}
	if prov.callCount() != 1 {
		t.Fatalf("expected 1 chat probe, got %d", prov.callCount())
	}
}

// Steady state: closed circuits produce no upstream traffic at all.
func TestCircuitProber_NoProbeWhenClosed(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}}

	newTestProber(health, prov).ProbeOnce(context.Background())

	if prov.callCount() != 0 || prov.modelHits != 0 {
		t.Fatalf("closed circuit must not be probed: chats=%d models=%d", prov.callCount(), prov.modelHits)
	}
}

// When the engine skips a provider because its circuit is open, the skip
// error (FinalError when all routes are skipped) carries the recorded cause.
func TestFallbackEngine_SkipErrorCarriesCause(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	health.RecordPersistentFailure("minimax", "test-model", "model", time.Hour)
	health.RecordCause("minimax", "test-model", "not_found", "provider not found: Model not exists")

	engine := NewFallbackEngine(health, router.FallbackConfig{})
	result := engine.ExecuteNonStream(context.Background(), makeRoutes("minimax"),
		func(_ context.Context, _ *router.RouteResult) (any, error) { return "ok", nil })

	if result.FinalError == nil {
		t.Fatal("expected skip error, got nil")
	}
	msg := result.FinalError.Error()
	if !strings.Contains(msg, "circuit breaker open") || !strings.Contains(msg, "not_found") ||
		!strings.Contains(msg, "Model not exists") {
		t.Fatalf("skip error must carry the circuit cause, got %q", msg)
	}
	// The upstream must never be called while the circuit is open.
	if result.Response != nil {
		t.Fatalf("open circuit must not reach upstream, got %v", result.Response)
	}
}

// The fallback engine records the upstream cause on the circuit it opens.
func TestFallbackEngine_RecordsCause(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Minute)
	engine := NewFallbackEngine(health, router.FallbackConfig{})
	routes := makeRoutes("minimax")

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		return nil, &provider.ProviderError{StatusCode: 404, ErrorType: provider.ErrorNotFound, Message: "provider not found: Model not exists"}
	})
	if result.FinalError == nil {
		t.Fatal("expected failure")
	}

	desc := health.OpenCircuitDescription("minimax", "")
	if desc == "" {
		t.Fatal("expected an open circuit with a description after 3 failures")
	}
	if !strings.Contains(desc, "not_found") || !strings.Contains(desc, "Model not exists") {
		t.Fatalf("description %q missing the recorded cause", desc)
	}
}
