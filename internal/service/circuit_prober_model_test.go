package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
)

// --- F-12 follow-up (review): model-scope circuits must NOT be closed by a
// healthy /models list (list-health != target-model inference health), and
// ProbeOnce must cap SIMULTANEOUS probes globally.

// A model-scope circuit whose target model still fails MUST stay open even
// though the /models endpoint answers 2xx — and the models probe is skipped
// entirely (no wasted call), with the decision made by the target-model chat.
func TestCircuitProber_ModelScopeRequiresChatEvenWhenModelsHealthy(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}}
	prov.outcomes = []bool{false} // target model chat FAILS (e.g. model dequota'd)
	health.RecordRateLimitFailure("minimax", "MiniMax-M3", 0) // opens name|model key

	p := newTestProber(health, prov)
	p.ProbeOnce(context.Background())

	if health.IsHealthyModel("minimax", "MiniMax-M3") {
		t.Fatal("model-scope circuit must NOT close on healthy /models while the target model fails")
	}
	if prov.modelHits != 0 {
		t.Fatalf("model-scope probe must skip the models call entirely, got %d hits", prov.modelHits)
	}
	if prov.callCount() != 1 {
		t.Fatalf("decision must come from exactly 1 target-model chat, got %d", prov.callCount())
	}

	// Target model recovers -> next pass closes via the real chat success.
	p.ProbeOnce(context.Background())
	if !health.IsHealthyModel("minimax", "MiniMax-M3") {
		t.Fatal("recovered target model must close the model-scope circuit")
	}
}

// An auth-failing chat probe (401 from the upstream) must keep the circuit
// open without extending openUntil — 401 is not model health either.
func TestCircuitProber_AuthFailingChatProbeStaysOpen(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"},
		listerErr: &provider.ProviderError{StatusCode: 404, ErrorType: provider.ErrorNotFound, Message: "404"}}
	prov.outcomes = []bool{false, false} // chat 401-ish failures
	health.RecordTransientFailure("minimax", "", 0)
	var until time.Time
	for _, s := range health.Snapshot() {
		if s.Model == "" {
			until = s.Until
		}
	}

	p := newTestProber(health, prov)
	p.ProbeOnce(context.Background())
	p.ProbeOnce(context.Background())

	if health.IsHealthy("minimax") {
		t.Fatal("auth-failing chat probe must not close the circuit")
	}
	for _, s := range health.Snapshot() {
		if s.Model == "" && !s.Until.Equal(until) {
			t.Fatalf("openUntil moved: %v -> %v", until, s.Until)
		}
	}
}

// sharedProbeMetrics records peak CONCURRENT chat probes ACROSS ALL circuits
// (the global bound is cross-provider, so the counter must be shared).
type sharedProbeMetrics struct {
	cur   atomic.Int32
	peak  atomic.Int32
	total atomic.Int32
}

type countingProv struct {
	name string
	m    *sharedProbeMetrics
}

func (p *countingProv) Name() string { return p.name }
func (p *countingProv) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	c := p.m.cur.Add(1)
	for {
		pk := p.m.peak.Load()
		if c <= pk || p.m.peak.CompareAndSwap(pk, c) {
			break
		}
	}
	defer p.m.cur.Add(-1)
	p.m.total.Add(1)
	time.Sleep(30 * time.Millisecond)
	return &domain.OpenAIResponse{}, nil
}

func (p *countingProv) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	return nil, nil
}

func TestCircuitProber_ProbeConcurrencyIsBounded(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	const n = 40 // far above probeConcurrency
	m := &sharedProbeMetrics{}
	reg := provider.NewRegistry()
	var names []string
	for i := 0; i < n; i++ {
		name := "p" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		names = append(names, name)
		reg.Register(name, &countingProv{name: name, m: m})
		health.RecordTransientFailure(name, "", 0)
	}
	row := &model.Provider{ID: 1, Name: names[0], Status: 1, APIKey: "sk-test"}
	NewCircuitProber(health, reg, probeRows{row}, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second, "").
		ProbeOnce(context.Background())

	if pk := m.peak.Load(); pk > int32(probeConcurrency) {
		t.Fatalf("peak concurrent probes %d exceeded global cap %d", pk, probeConcurrency)
	}
	if tot := m.total.Load(); tot != n {
		t.Fatalf("expected exactly %d probes in the pass, got %d", n, tot)
	}
	for _, name := range names {
		if !health.IsHealthy(name) {
			t.Fatalf("%s: successful probe must close its circuit", name)
		}
	}
}

// --- review: the probe timeout must cover the WHOLE chain (row lookup +
// secret resolution + probes) and semaphore waits must be cancellable.

// blockingRows blocks GetByName until the gate closes (simulates a slow repo).
type blockingRows struct {
	probeRows
	gate chan struct{}
}

func (r *blockingRows) GetByName(ctx context.Context, _ string) (*model.Provider, error) {
	select {
	case <-r.gate:
		return r.row, nil
	case <-ctx.Done(): // a real repo honors ctx; the probe timeout must reach it
		return nil, ctx.Err()
	}
}

// The full-chain timeout bounds a slow provider-row lookup: the probe pass
// returns well before the gate ever opens and the circuit stays open.
func TestCircuitProber_SlowRepoIsBoundedByTimeout(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}}
	prov.outcomes = []bool{false}
	health.RecordTransientFailure("minimax", "", 0)
	reg := provider.NewRegistry()
	reg.Register(prov.Name(), prov)
	br := &blockingRows{probeRows: probeRows{row: &model.Provider{ID: 1, Name: "minimax", Status: 1, APIKey: "sk"}}, gate: make(chan struct{})}
	defer close(br.gate)
	p := NewCircuitProber(health, reg, br, probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second, "/models")

	done := make(chan struct{})
	start := time.Now()
	go func() { p.ProbeOnce(context.Background()); close(done) }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 12*time.Second {
			t.Fatalf("slow repo not bounded: pass took %v", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("probe pass hung on a slow repo — timeout does not cover the row lookup")
	}
	if health.IsHealthy("minimax") {
		t.Fatal("timed-out probe must not close the circuit")
	}
	if prov.callCount() != 0 {
		t.Fatalf("no chat should happen after a timed-out lookup, got %d", prov.callCount())
	}
}

// blockingSecrets blocks Resolve until the gate closes (slow secret backend).
type blockingSecrets struct{ gate chan struct{} }

func (s *blockingSecrets) Resolve(ctx context.Context, _ string) (string, error) {
	select {
	case <-s.gate:
		return "resolved", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestCircuitProber_SlowSecretIsBoundedByTimeout(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	prov := &probeProvider{scriptedProvider: scriptedProvider{name: "minimax"}}
	prov.outcomes = []bool{false}
	health.RecordTransientFailure("minimax", "", 0)
	reg := provider.NewRegistry()
	reg.Register(prov.Name(), prov)
	bs := &blockingSecrets{gate: make(chan struct{})}
	defer close(bs.gate)
	p := NewCircuitProber(health, reg, probeRows{row: &model.Provider{ID: 1, Name: "minimax", Status: 1, APIKey: "ref://x"}},
		probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, bs, time.Second, "/models")

	done := make(chan struct{})
	start := time.Now()
	go func() { p.ProbeOnce(context.Background()); close(done) }()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 12*time.Second {
			t.Fatalf("slow secret not bounded: %v", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("probe pass hung on a slow secret backend — timeout does not cover secret resolution")
	}
	if health.IsHealthy("minimax") {
		t.Fatal("timed-out probe must not close the circuit")
	}
}

// A cancelled context must unblock probes WAITING on the full semaphore: 8
// blocked chats fill the slots, remaining circuits queue, cancel drains the
// pass promptly with at most probeConcurrency probes ever started.
func TestCircuitProber_SemaphoreWaitIsCancellable(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Hour)
	gate := make(chan struct{})
	defer close(gate)
	m := &sharedProbeMetrics{}
	reg := provider.NewRegistry()
	const n = 40
	var names []string
	for i := 0; i < n; i++ {
		name := "q" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		names = append(names, name)
		reg.Register(name, &gatedProv{name: name, m: m, gate: gate})
		health.RecordTransientFailure(name, "", 0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewCircuitProber(health, reg, probeRows{row: &model.Provider{ID: 1, Name: names[0], Status: 1, APIKey: "sk"}},
		probeModels{first: &model.ProviderModel{ProviderModel: "pm-1"}}, nil, time.Second, "")

	done := make(chan struct{})
	go func() { p.ProbeOnce(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond) // let the 8 slots fill and block
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled pass did not return — semaphore wait is not cancellable")
	}
	if started := m.total.Load(); started > int32(probeConcurrency) {
		t.Fatalf("started %d probes, cap is %d", started, probeConcurrency)
	}
}

// gatedProv blocks in Chat until gate closes; counts into shared metrics.
type gatedProv struct {
	name string
	m    *sharedProbeMetrics
	gate chan struct{}
}

func (p *gatedProv) Name() string { return p.name }
func (p *gatedProv) Chat(_ context.Context, _ *domain.OpenAIRequest, _ string) (*domain.OpenAIResponse, error) {
	p.m.total.Add(1)
	<-p.gate
	return &domain.OpenAIResponse{}, nil
}
func (p *gatedProv) StreamChat(_ context.Context, _ *domain.OpenAIRequest, _ string) (<-chan domain.SSEChunk, error) {
	return nil, nil
}
