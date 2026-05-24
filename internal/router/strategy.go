package router

import (
	"context"
	"encoding/json"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
)

// LatencyProvider fetches average latency for a provider.
// Implemented by service.LatencyService.
type LatencyProvider interface {
	GetAvgLatency(ctx context.Context, providerName string) float64
}

type StrategyName string

const (
	StrategyWeightedRandom StrategyName = "weighted_random"
	StrategyRoundRobin     StrategyName = "round_robin"
	StrategyLeastLatency   StrategyName = "least_latency"
	StrategyLeastCost      StrategyName = "least_cost"
	StrategyCanary         StrategyName = "canary"
	StrategyLeastBusy      StrategyName = "least_busy"
)

// GateChecker is implemented by license.Gate to check tier access.
// nil GateChecker means all strategies are valid (Community/NoopGate behavior).
type GateChecker interface {
	RequirePro() error
	RequireEnterprise() error
}

// IsValidStrategy checks if a strategy is available for the current license tier.
// Pass nil gate to allow all known strategies (Community/NoopGate behavior).
func IsValidStrategy(name StrategyName, gate GateChecker) bool {
	if gate == nil {
		switch name {
		case StrategyWeightedRandom, StrategyRoundRobin, StrategyLeastLatency,
			StrategyLeastCost, StrategyCanary, StrategyLeastBusy:
			return true
		}
		return false
	}
	switch name {
	case StrategyWeightedRandom, StrategyRoundRobin:
		return true
	case StrategyLeastLatency, StrategyLeastBusy:
		return gate.RequirePro() == nil
	case StrategyLeastCost, StrategyCanary:
		return gate.RequireEnterprise() == nil
	}
	return false
}

type RouteCandidate struct {
	Provider      provider.Provider
	ProviderModel string
	ProviderRow   *model.Provider
	InputPrice    float64
	OutputPrice   float64
	Currency      string
	Weight        int
	Priority      int
	ModelID       int64
	ModelName     string
	AvgLatencyMs   float64
	CanaryPercent  int
	ActiveRequests int64
	RetryConfig    provider.RetryConfig
	FallbackModels []string
	FallbackCfg    FallbackConfig
	ExtraConfig    json.RawMessage
}

type RoutingStrategy interface {
	Select(ctx context.Context, candidates []RouteCandidate) (picked *RouteCandidate, ordered []*RouteResult)
	Name() StrategyName
}

func splitPrimariesFallbacks(candidates []RouteCandidate) (primaries, fallbacks []RouteCandidate) {
	for _, c := range candidates {
		if c.Weight > 0 {
			primaries = append(primaries, c)
		} else {
			fallbacks = append(fallbacks, c)
		}
	}
	return
}

func candidateToRouteResult(c RouteCandidate) *RouteResult {
	return &RouteResult{
		Provider:       c.Provider,
		ProviderModel:  c.ProviderModel,
		InputPrice:     c.InputPrice,
		OutputPrice:    c.OutputPrice,
		Currency:       c.Currency,
		ProviderRow:    c.ProviderRow,
		RetryConfig:    c.RetryConfig,
		FallbackModels: c.FallbackModels,
		FallbackConfig: c.FallbackCfg,
		ExtraConfig:    c.ExtraConfig,
	}
}

func candidatesToRouteResults(cs []RouteCandidate) []*RouteResult {
	out := make([]*RouteResult, len(cs))
	for i, c := range cs {
		out[i] = candidateToRouteResult(c)
	}
	return out
}

func parseCanaryPercent(extraConfig json.RawMessage) int {
	if len(extraConfig) == 0 {
		return 0
	}
	var cfg struct {
		CanaryPercent int `json:"canary_percent"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil {
		return 0
	}
	if cfg.CanaryPercent < 0 || cfg.CanaryPercent > 100 {
		return 0
	}
	return cfg.CanaryPercent
}

// FallbackConfig controls fine-grained fallback behavior per model mapping.
type FallbackConfig struct {
	MaxRetries   int      `json:"max_retries"`
	RetryOn      []string `json:"retry_on"`
	RetryDelayMs int      `json:"retry_delay_ms"`
}

// extraConfig holds all fields parsed from a model's ExtraConfig JSON.
type extraConfig struct {
	CanaryPercent  int            `json:"canary_percent"`
	NumRetries     int            `json:"num_retries"`
	BackoffType    string         `json:"backoff_type"`
	InitialMs      int            `json:"initial_ms"`
	MaxBackoffMs   int            `json:"max_backoff_ms"`
	FallbackModels []string       `json:"fallback_models"`
	Fallback       FallbackConfig `json:"fallback"`
	Guardrails     guardrail.ModelGuardrailConfig `json:"guardrails"`
}

func parseExtraConfig(raw json.RawMessage) extraConfig {
	var cfg extraConfig
	if len(raw) == 0 {
		return cfg
	}
	json.Unmarshal(raw, &cfg)
	if cfg.CanaryPercent < 0 || cfg.CanaryPercent > 100 {
		cfg.CanaryPercent = 0
	}
	return cfg
}

func parseFallbackModels(extraConfig json.RawMessage) []string {
	if len(extraConfig) == 0 {
		return nil
	}
	var cfg struct {
		FallbackModels []string `json:"fallback_models"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil {
		return nil
	}
	return cfg.FallbackModels
}
