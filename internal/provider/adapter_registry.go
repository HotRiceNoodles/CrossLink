package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/crosslink/internal/model"
)

type AdapterFactory func(p *model.Provider, timeout time.Duration) (Provider, error)

type AdapterMeta struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// AdapterVersion tracks the adapter's behavior revision independently of
	// the gateway release — bump it whenever the adapter's wire behavior
	// changes (translation, auth, retry semantics), so "same gateway version,
	// different provider behavior" is diagnosable from system info.
	AdapterVersion   string            `json:"adapter_version"`
	NeedsBaseURL     bool              `json:"needs_base_url"`
	NeedsAPIKey      bool              `json:"needs_api_key"`
	BaseURLDefault   string            `json:"base_url_default,omitempty"`
	ProtocolBaseURLs map[string]string `json:"protocol_base_urls,omitempty"`
	Capabilities     []string          `json:"capabilities"`
	ExtraFields      []AdapterField    `json:"extra_fields"`
	MinimumTier      string            `json:"minimum_tier,omitempty"` // "community", "pro", "enterprise"
}

type AdapterField struct {
	Name         string        `json:"name"`
	Label        string        `json:"label"`
	Type         string        `json:"type"` // text, password, select, number, textarea
	Required     bool          `json:"required"`
	Placeholder  string        `json:"placeholder"`
	DefaultValue string        `json:"default_value"`
	Options      []FieldOption `json:"options,omitempty"`
	Secret       bool          `json:"secret"`
	// ShowWhen conditionally renders the field in the dynamic form: a
	// "key=value" condition evaluated against the current extra_config draft
	// (e.g. cloud shells exposing api_protocol=anthropic show api_version
	// only for that protocol). Presentation only — the backend validates
	// unconditionally.
	ShowWhen string `json:"show_when,omitempty"`
}

type FieldOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

var (
	adapterFactories = map[string]AdapterFactory{}
	adapterMetas     = map[string]*AdapterMeta{}
)

func RegisterAdapter(adapterType string, factory AdapterFactory, meta *AdapterMeta) {
	adapterFactories[adapterType] = factory
	adapterMetas[adapterType] = meta
}

func CreateProvider(p *model.Provider, timeout time.Duration) (Provider, error) {
	factory, ok := adapterFactories[p.AdapterType]
	if !ok {
		return nil, fmt.Errorf("unsupported adapter_type: %s", p.AdapterType)
	}
	return factory(p, timeout)
}

type AdapterInfo struct {
	Type string       `json:"type"`
	Meta *AdapterMeta `json:"meta"`
}

func ListAdapters() []AdapterInfo {
	list := make([]AdapterInfo, 0, len(adapterMetas))
	for typ, meta := range adapterMetas {
		list = append(list, AdapterInfo{Type: typ, Meta: meta})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Type < list[j].Type })
	return list
}

// GetAdapterMeta returns the metadata for a registered adapter type, or nil if not found.
func GetAdapterMeta(adapterType string) *AdapterMeta {
	return adapterMetas[adapterType]
}

// AdapterServesResponses reports whether a provider row can serve the 3A
// raw-passthrough path (/v1/responses) at all — the write-side gate that
// keeps the model-level supports_responses flag from being configured where
// it can only silently no-op (runtime keeps its own double gate: flag AND
// the ResponsesProvider interface assertion).
//
// Rules, most specific first:
//   - the native anthropic adapter never serves Responses;
//   - an api_protocol-switched shell (cloud shells) in anthropic mode never does;
//   - a registered adapter that declares the "responses" capability does
//     (openai_compatible, ollama — mirrors of their ResponsesProvider impls);
//   - a registered adapter with an api_protocol switch left in openai mode does;
//   - a registered adapter with NEITHER (azure_openai, mock in community)
//     does not;
//   - an unregistered type (an overlay shell under a community build, where
//     its factory never ran) is allowed — the runtime gate decides.
func AdapterServesResponses(adapterType string, extraConfig []byte) bool {
	if adapterType == "anthropic" || extractProtocol(extraConfig) == "anthropic" {
		return false
	}
	meta := GetAdapterMeta(adapterType)
	if meta == nil {
		return true
	}
	for _, c := range meta.Capabilities {
		if c == "responses" {
			return true
		}
	}
	for _, f := range meta.ExtraFields {
		if f.Name == "api_protocol" {
			return true // protocol-switched shell; anthropic mode already rejected above
		}
	}
	return false
}

// extractProtocol reads api_protocol from ExtraConfig JSON, defaults to "openai".
func extractProtocol(extraConfig []byte) string {
	if len(extraConfig) == 0 {
		return "openai"
	}
	var cfg struct {
		APIProtocol string `json:"api_protocol"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil || cfg.APIProtocol == "" {
		return "openai"
	}
	return cfg.APIProtocol
}

// Bounds for the provider-level http_timeout_seconds key (M4): the
// NON-STREAMING client total timeout (http.Client.Timeout). Streaming keeps
// its own client (newStreamClient — no total timeout, idle-watchdog +
// per-request context instead), and admin probes (Test/ListRemoteModels)
// keep their fixed 10s/15s budgets — this key configures neither.
const (
	minHTTPTimeoutSeconds = 5
	maxHTTPTimeoutSeconds = 600
)

// ValidateHTTPTimeout checks the http_timeout_seconds key of an extra_config
// JSON (M4): absent is valid (callers use their fallback); when present it
// must be a number within [5, 600]. A document that fails to unmarshal (wrong
// type on our key, malformed JSON) is rejected — unlike an absent key, a
// present-but-garbage value must fail the write. Meant to run BEFORE
// persisting provider rows, alongside ValidateAnthropicAPIVersion.
func ValidateHTTPTimeout(extraConfig []byte) error {
	if len(extraConfig) == 0 {
		return nil
	}
	var cfg struct {
		HTTPTimeoutSeconds json.Number `json:"http_timeout_seconds"`
	}
	if err := json.Unmarshal(extraConfig, &cfg); err != nil {
		return fmt.Errorf("http_timeout_seconds: invalid extra_config JSON: %w", err)
	}
	if cfg.HTTPTimeoutSeconds == "" {
		return nil
	}
	sec, err := cfg.HTTPTimeoutSeconds.Float64()
	if err != nil {
		return fmt.Errorf("http_timeout_seconds must be a number (got %q)", cfg.HTTPTimeoutSeconds.String())
	}
	if sec < minHTTPTimeoutSeconds || sec > maxHTTPTimeoutSeconds {
		return fmt.Errorf("http_timeout_seconds must be within [%d, %d] seconds (non-streaming client total timeout)", minHTTPTimeoutSeconds, maxHTTPTimeoutSeconds)
	}
	return nil
}

// ResolveHTTPTimeout is the runtime read side of ValidateHTTPTimeout: every
// construction path (startup registration, admin CRUD sync, wizard commit,
// RegistrySync remote reload) derives the non-streaming client timeout from
// the provider's own extra_config, falling back when the key is absent. An
// out-of-range value that slipped past write-time validation degrades to the
// fallback with a warning instead of constructing a mis-behaving client.
func ResolveHTTPTimeout(extraConfig []byte, fallback time.Duration) time.Duration {
	if len(extraConfig) == 0 {
		return fallback
	}
	var cfg struct {
		HTTPTimeoutSeconds json.Number `json:"http_timeout_seconds"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil || cfg.HTTPTimeoutSeconds == "" {
		return fallback
	}
	sec, err := cfg.HTTPTimeoutSeconds.Float64()
	if err != nil || sec < minHTTPTimeoutSeconds || sec > maxHTTPTimeoutSeconds {
		slog.Warn("ignoring invalid http_timeout_seconds, using fallback",
			"value", cfg.HTTPTimeoutSeconds.String(), "fallback_seconds", fallback/time.Second)
		return fallback
	}
	return time.Duration(sec * float64(time.Second))
}

// Bounds for the three poll knobs (图片方案 P1, design §3.5 评审 F4): the
// adapter-internal async bridge (PollUntil) tuning keys. Ranges mirror
// PollConfigFromExtra's runtime acceptance so a row that passes write-time
// validation behaves exactly as configured.
const (
	minPollTimeoutSeconds = 10
	maxPollTimeoutSeconds = 600
	minPollIntervalMs     = 500
	maxPollIntervalMs     = 30000
	minPollMaxIntervalMs  = 500
	maxPollMaxIntervalMs  = 60000
)

// ValidatePollConfig checks the poll_* keys of an extra_config JSON (图片方案
// P1, design §3.5 评审 F4): absent keys are valid (callers use defaults); when
// present each must be a number within its legal range, and poll_max_interval_ms
// must not be below poll_interval_ms when both are set. Meant to run BEFORE
// persisting provider rows, alongside ValidateHTTPTimeout — without it an
// out-of-range knob is silently ignored at runtime (PollConfigFromExtra falls
// back to the default) and the misconfiguration is undiagnosable.
func ValidatePollConfig(extraConfig []byte) error {
	if len(extraConfig) == 0 {
		return nil
	}
	var cfg struct {
		PollTimeoutSeconds json.Number `json:"poll_timeout_seconds"`
		PollIntervalMs     json.Number `json:"poll_interval_ms"`
		PollMaxIntervalMs  json.Number `json:"poll_max_interval_ms"`
	}
	if err := json.Unmarshal(extraConfig, &cfg); err != nil {
		return fmt.Errorf("poll_*: invalid extra_config JSON: %w", err)
	}
	var interval, maxIv float64
	if cfg.PollTimeoutSeconds != "" {
		v, err := cfg.PollTimeoutSeconds.Float64()
		if err != nil {
			return fmt.Errorf("poll_timeout_seconds must be a number (got %q)", cfg.PollTimeoutSeconds.String())
		}
		if v < minPollTimeoutSeconds || v > maxPollTimeoutSeconds {
			return fmt.Errorf("poll_timeout_seconds must be within [%d, %d] seconds (async poll total budget)", minPollTimeoutSeconds, maxPollTimeoutSeconds)
		}
	}
	if cfg.PollIntervalMs != "" {
		v, err := cfg.PollIntervalMs.Float64()
		if err != nil {
			return fmt.Errorf("poll_interval_ms must be a number (got %q)", cfg.PollIntervalMs.String())
		}
		if v < minPollIntervalMs || v > maxPollIntervalMs {
			return fmt.Errorf("poll_interval_ms must be within [%d, %d] (initial poll sleep)", minPollIntervalMs, maxPollIntervalMs)
		}
		interval = v
	}
	if cfg.PollMaxIntervalMs != "" {
		v, err := cfg.PollMaxIntervalMs.Float64()
		if err != nil {
			return fmt.Errorf("poll_max_interval_ms must be a number (got %q)", cfg.PollMaxIntervalMs.String())
		}
		if v < minPollMaxIntervalMs || v > maxPollMaxIntervalMs {
			return fmt.Errorf("poll_max_interval_ms must be within [%d, %d] (backoff cap)", minPollMaxIntervalMs, maxPollMaxIntervalMs)
		}
		maxIv = v
	}
	if cfg.PollIntervalMs != "" && cfg.PollMaxIntervalMs != "" && maxIv < interval {
		return fmt.Errorf("poll_max_interval_ms (%s) must be >= poll_interval_ms (%s) — a cap below the initial sleep is degenerate", cfg.PollMaxIntervalMs.String(), cfg.PollIntervalMs.String())
	}
	return nil
}

// ValidateCommonExtraConfig runs the pre-persist validations shared by every
// provider write path (admin CRUD, onboarding wizard, import): the
// adapter-agnostic keys every protocol implementation honors. Add new
// version-gated or range-checked keys here so all entries fail identically
// before a row is persisted.
func ValidateCommonExtraConfig(extraConfig []byte) error {
	if err := ValidateAnthropicAPIVersion(extraConfig); err != nil {
		return err
	}
	if err := ValidateHTTPTimeout(extraConfig); err != nil {
		return err
	}
	return ValidatePollConfig(extraConfig)
}

// PollConfigFromExtra reads the three poll knobs (图片方案 P1) from a
// provider's extra_config. Defaults: 120s / 3s / 5s. Out-of-range values
// fall back to the default with a warning (defense-in-depth: write-time
// validation already rejects them, but a stale row written before validation
// existed must not produce a mis-behaving loop — and must not fail silently).
//
// Values arrive as json.Number (NOT int): the admin form seeds untouched
// defaults into extra_config as JSON strings ("120" not 120), the same wire
// quirk ResolveHTTPTimeout already tolerates — an int-typed struct would
// fail the whole unmarshal on the first quoted value and silently discard
// every knob.
func PollConfigFromExtra(extraConfig []byte) PollConfig {
	cfg := PollConfig{
		Timeout:     120 * time.Second,
		Interval:    3 * time.Second,
		MaxInterval: 5 * time.Second,
	}
	if len(extraConfig) == 0 {
		return cfg
	}
	var v struct {
		TimeoutSec    json.Number `json:"poll_timeout_seconds"`
		IntervalMs    json.Number `json:"poll_interval_ms"`
		MaxIntervalMs json.Number `json:"poll_max_interval_ms"`
	}
	if json.Unmarshal(extraConfig, &v) != nil {
		return cfg
	}
	if sec, ok := numIn(v.TimeoutSec, minPollTimeoutSeconds, maxPollTimeoutSeconds); ok {
		cfg.Timeout = time.Duration(sec) * time.Second
	} else if v.TimeoutSec != "" {
		slog.Warn("ignoring invalid poll_timeout_seconds, using default", "value", v.TimeoutSec.String())
	}
	if ms, ok := numIn(v.IntervalMs, minPollIntervalMs, maxPollIntervalMs); ok {
		cfg.Interval = time.Duration(ms) * time.Millisecond
	} else if v.IntervalMs != "" {
		slog.Warn("ignoring invalid poll_interval_ms, using default", "value", v.IntervalMs.String())
	}
	if v.MaxIntervalMs != "" {
		if ms, ok := numIn(v.MaxIntervalMs, minPollMaxIntervalMs, maxPollMaxIntervalMs); ok && ms >= float64(cfg.Interval.Milliseconds()) {
			cfg.MaxInterval = time.Duration(ms) * time.Millisecond
		} else {
			slog.Warn("ignoring invalid poll_max_interval_ms, capping at interval (no backoff)", "value", v.MaxIntervalMs.String())
			cfg.MaxInterval = cfg.Interval // degenerate: cap at interval (no backoff)
		}
	} // absent (not set): keep default
	return cfg
}

// numIn parses an extra_config json.Number (tolerating the quoted-string form
// the admin form emits for untouched defaults) and reports whether it lies
// within [min, max]. Absent/empty or non-numeric → (0, false).
func numIn(n json.Number, min, max int) (float64, bool) {
	if n == "" {
		return 0, false
	}
	v, err := n.Float64()
	if err != nil {
		return 0, false
	}
	if v < float64(min) || v > float64(max) {
		return 0, false
	}
	return v, true
}
