package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	Provider       provider.Provider
	ProviderModel  string
	ProviderRow    *model.Provider
	InputPrice     float64
	OutputPrice    float64
	Currency       string
	Weight         int
	Priority       int
	ModelID        int64
	ModelName      string
	AvgLatencyMs   float64
	CanaryPercent  int
	ActiveRequests int64
	RetryConfig    provider.RetryConfig
	FallbackModels []string
	FallbackCfg    FallbackConfig
	ExtraConfig    json.RawMessage
	MaxContext     *int
	// M3.2/P3 model-level extension controls (see RouteResult).
	RequestOverrides  map[string]any
	AllowedExtensions []string
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
		Provider:          c.Provider,
		ProviderModel:     c.ProviderModel,
		InputPrice:        c.InputPrice,
		OutputPrice:       c.OutputPrice,
		Currency:          c.Currency,
		ProviderRow:       c.ProviderRow,
		RetryConfig:       c.RetryConfig,
		FallbackModels:    c.FallbackModels,
		FallbackConfig:    c.FallbackCfg,
		ExtraConfig:       c.ExtraConfig,
		Weight:            c.Weight,
		Priority:          c.Priority,
		MaxContext:        c.MaxContext,
		RequestOverrides:  c.RequestOverrides,
		AllowedExtensions: c.AllowedExtensions,
	}
}

// ModelOverrideFields is the allow-table for request_overrides keys (M3.2):
// generation params an admin may inject/override per model mapping. The
// plan's deny-list (model/messages/tools/tool_choice/stream/stream_options/
// max_tokens/max_completion_tokens/auth) is enforced by NOT being here —
// "其他字段也须在允许表内;非法配置不能静默忽略".
var ModelOverrideFields = map[string]string{
	"temperature":     "number",
	"top_p":           "number",
	"enable_thinking": "bool",
}

// DialectExtensionFields is the allow-table for allowed_extensions opt-ins:
// registered admission dialect params a route may forward upstream.
var DialectExtensionFields = map[string]bool{
	"enable_thinking": true,
	// Phase-1 effort admission (2026-11-25 checklist §6.1): anthropic-inbound
	// output_config.effort, forwarded on opted-in routes — natively on
	// anthropic upstreams, translated to reasoning_effort on openai upstreams.
	"output_config": true,
}

// ValidateModelExtensionConfig validates the request_overrides and
// allowed_extensions keys of a model mapping's extra_config BEFORE the row is
// persisted (M3.2 "配置写入和实际执行双重校验" — write half). Values are
// shape-checked against the allow-table types; the execution half re-checks
// at dispatch.
func ValidateModelExtensionConfig(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var cfg struct {
		RequestOverrides  map[string]any `json:"request_overrides"`
		AllowedExtensions []string       `json:"allowed_extensions"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil // malformed extra_config as a whole is not this validator's lane
	}
	for k, v := range cfg.RequestOverrides {
		wantType, ok := ModelOverrideFields[k]
		if !ok {
			return fmt.Errorf("request_overrides: %q is not an overridable generation param (allowed: temperature, top_p, enable_thinking)", k)
		}
		if !matchesOverrideShape(v, wantType) {
			return fmt.Errorf("request_overrides: %q must be a %s", k, wantType)
		}
	}
	for _, k := range cfg.AllowedExtensions {
		if !DialectExtensionFields[k] {
			allowed := make([]string, 0, len(DialectExtensionFields))
			for name := range DialectExtensionFields {
				allowed = append(allowed, name)
			}
			sort.Strings(allowed)
			return fmt.Errorf("allowed_extensions: %q is not a registered dialect param (allowed: %s)", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func matchesOverrideShape(v any, want string) bool {
	switch want {
	case "number":
		if _, ok := v.(float64); !ok {
			return false
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return false
		}
	case "string":
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return true
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
	CanaryPercent  int                            `json:"canary_percent"`
	NumRetries     int                            `json:"num_retries"`
	BackoffType    string                         `json:"backoff_type"`
	InitialMs      int                            `json:"initial_ms"`
	MaxBackoffMs   int                            `json:"max_backoff_ms"`
	FallbackModels []string                       `json:"fallback_models"`
	Fallback       FallbackConfig                 `json:"fallback"`
	Guardrails     guardrail.ModelGuardrailConfig `json:"guardrails"`
	// P3a provider guardrails (opt-in, 0 = disabled). Read by the dispatch
	// path (handler callFn) via AcquireDispatchGuard.
	Concurrency int `json:"concurrency"` // max in-flight requests per (provider,model)
	RPM         int `json:"rpm"`         // max requests per minute per (provider,model)
	// M3.2/P3: model-level extension controls. RequestOverrides are admin
	// injection/override of ALLOWED generation params (validated at write
	// time by ValidateModelExtensionConfig); AllowedExtensions opts a route
	// in to forwarding registered dialect params (cost-bearing: enable_thinking
	// bills thinking tokens as output).
	RequestOverrides  map[string]any `json:"request_overrides"`
	AllowedExtensions []string       `json:"allowed_extensions"`
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

// ParseGuardrailConfig extracts the P3a provider guardrail limits (concurrency,
// rpm) from a model's ExtraConfig JSON. Returns (0, 0) when absent or unparseable.
// Used by the dispatch path to feed service.AcquireDispatchGuard without
// exposing the private extraConfig struct.
func ParseGuardrailConfig(raw json.RawMessage) (concurrency, rpm int) {
	cfg := parseExtraConfig(raw)
	return cfg.Concurrency, cfg.RPM
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
