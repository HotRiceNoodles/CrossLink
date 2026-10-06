package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// probeProvider counts calls and controls the outcome of Chat. lister controls
// whether it advertises a /models endpoint (and whether that succeeds).
type probeProvider struct {
	scriptedProvider
	lister    bool
	listerErr error
	modelHits int
}

func (p *probeProvider) ListUpstreamModels(_ context.Context, _ string) ([]provider.UpstreamModel, error) {
	p.modelHits++
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
	reg := provider.NewRegistry()
	reg.Register(prov.Name(), prov)
	row := &model.Provider{ID: 1, Name: prov.Name(), Status: 1, APIKey: "sk-test"}
	return NewCircuitProber(health, reg, probeRows{row}, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second)
}

// A successful /models probe closes an open circuit early.
func TestCircuitProber_ModelsProbeClosesCircuit(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}, lister: true}
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
}

// A failing /models probe leaves the circuit exactly as it was — openUntil
// must not move (a prober failure must never extend the cooldown).
func TestCircuitProber_FailedProbeLeavesStateUntouched(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}, lister: true,
		listerErr: context.DeadlineExceeded}
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
		t.Fatal("failed probe must not close the circuit")
	}
	for _, s := range health.Snapshot() {
		if s.Model == "" && !s.Until.Equal(until) {
			t.Fatalf("openUntil moved after failed probes: %v -> %v", until, s.Until)
		}
	}
}

// Without a /models endpoint, the prober falls back to a 1-token chat on the
// circuit's model and closes on success.
func TestCircuitProber_ChatFallbackClosesCircuit(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	// scriptedProvider has no ListUpstreamModels method → chat fallback path.
	prov := &scriptedProvider{name: "minimax"}
	health.RecordPersistentFailure("minimax", "MiniMax-M3", "model", time.Hour)

	reg := provider.NewRegistry()
	reg.Register("minimax", prov)
	row := &model.Provider{ID: 1, Name: "minimax", Status: 1, APIKey: "sk-test"}
	NewCircuitProber(health, reg, probeRows{row}, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second).
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
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}, lister: true}

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
