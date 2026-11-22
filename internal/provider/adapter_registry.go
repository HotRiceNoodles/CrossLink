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

// ValidateCommonExtraConfig runs the pre-persist validations shared by every
// provider write path (admin CRUD, onboarding wizard, import): the
// adapter-agnostic keys every protocol implementation honors. Add new
// version-gated or range-checked keys here so all entries fail identically
// before a row is persisted.
func ValidateCommonExtraConfig(extraConfig []byte) error {
	if err := ValidateAnthropicAPIVersion(extraConfig); err != nil {
		return err
	}
	return ValidateHTTPTimeout(extraConfig)
}
