package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

// CircuitProber closes open circuits as soon as the upstream verifiably
// recovers, instead of waiting out the full cooldown. Every interval it scans
// the health snapshot for OPEN circuits and sends one cheap probe per circuit:
// /models via ModelsLister when the adapter supports it (free), otherwise a
// 1-token chat. A successful probe calls HealthTracker.ResolveCircuit — the
// documented C1 exception for an out-of-band authoritative recovery signal.
//
// A FAILED probe only logs: it must never call Record*Failure, which would
// push openUntil forward and turn the cooldown into an indefinite block.
type CircuitProber struct {
	health   *provider.HealthTracker
	registry *provider.Registry
	rows     ProberProviderSource
	models   ProberModelSource
	secrets  ProberSecretResolver
	interval time.Duration
}

// ProberProviderSource is the slice of ProviderRepo the prober needs.
type ProberProviderSource interface {
	GetByName(ctx context.Context, name string) (*model.Provider, error)
}

// ProberModelSource is the slice of ProviderModelCRUDRepo the prober needs
// (model name for 1-token chat probes on account-scope circuits).
type ProberModelSource interface {
	FirstByProviderID(ctx context.Context, providerID int64) (*model.ProviderModel, error)
}

// ProberSecretResolver is the slice of SecretResolver the prober needs; nil
// disables reference resolution (plaintext keys pass through).
type ProberSecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

func NewCircuitProber(
	health *provider.HealthTracker,
	registry *provider.Registry,
	rows ProberProviderSource,
	models ProberModelSource,
	secrets ProberSecretResolver,
	interval time.Duration,
) *CircuitProber {
	if interval < 3*time.Second {
		interval = 3 * time.Second
	}
	return &CircuitProber{
		health:   health,
		registry: registry,
		rows:     rows,
		models:   models,
		secrets:  secrets,
		interval: interval,
	}
}

// Run probes open circuits every interval until ctx is cancelled.
func (p *CircuitProber) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeAll(ctx)
		}
	}
}

// ProbeOnce runs a single probe pass — exported for tests and manual triggers.
func (p *CircuitProber) ProbeOnce(ctx context.Context) { p.probeAll(ctx) }

func (p *CircuitProber) probeAll(ctx context.Context) {
	open := 0
	for _, s := range p.health.Snapshot() {
		if s.State != provider.CircuitOpen.String() {
			continue
		}
		open++
	}
	if open == 0 {
		return // steady state: zero upstream traffic
	}
	var wg sync.WaitGroup
	for _, s := range p.health.Snapshot() {
		if s.State != provider.CircuitOpen.String() {
			continue
		}
		wg.Add(1)
		go func(s provider.ProviderHealthSnapshot) {
			defer wg.Done()
			p.probeOne(ctx, s)
		}(s)
	}
	wg.Wait()
}

func (p *CircuitProber) probeOne(ctx context.Context, s provider.ProviderHealthSnapshot) {
	prov, ok := p.registry.Get(s.Provider)
	if !ok {
		return // provider not registered (removed/renamed) — nothing to probe
	}
	row, err := p.rows.GetByName(ctx, s.Provider)
	if err != nil {
		slog.Warn("circuit prober: provider row not found", "provider", s.Provider, "error", err)
		return
	}
	apiKey := row.APIKey
	if p.secrets != nil {
		resolved, err := p.secrets.Resolve(ctx, apiKey)
		if err != nil {
			slog.Warn("circuit prober: secret resolution failed", "provider", s.Provider, "error", err)
			return
		}
		apiKey = resolved
	}

	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if lister, ok := prov.(provider.ModelsLister); ok {
		if _, err := lister.ListUpstreamModels(pctx, apiKey); err != nil {
			slog.Warn("circuit prober: /models probe failed, circuit stays open",
				"provider", s.Provider, "model", s.Model, "error", err)
			return
		}
		p.close(s)
		return
	}

	// No /models endpoint: verify with a 1-token chat on the circuit's model
	// (model-scope circuit) or the provider's first active mapping.
	modelName := s.Model
	if modelName == "" && p.models != nil {
		if m, err := p.models.FirstByProviderID(pctx, row.ID); err == nil {
			modelName = m.ProviderModel
		}
	}
	if modelName == "" {
		return // nothing to probe with
	}
	maxTokens := 1
	req := &domain.OpenAIRequest{
		Model:     modelName,
		MaxTokens: &maxTokens,
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	if _, err := prov.Chat(pctx, req, apiKey); err != nil {
		slog.Warn("circuit prober: chat probe failed, circuit stays open",
			"provider", s.Provider, "model", modelName, "error", err)
		return
	}
	p.close(s)
}

func (p *CircuitProber) close(s provider.ProviderHealthSnapshot) {
	p.health.ResolveCircuit(s.Provider, s.Model)
	slog.Info("circuit prober: upstream recovered, circuit closed early",
		"provider", s.Provider, "model", s.Model)
}
