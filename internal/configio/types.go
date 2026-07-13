package configio

import "time"

// BundleVersion is the export format version. Bump when the schema changes;
// Decrypt dispatches by magic, ApplyBundle can branch on this if needed.
const BundleVersion = "1"

// ExportBundle is the plaintext YAML payload encrypted inside a CLCFG file.
type ExportBundle struct {
	Version    string             `yaml:"version"`
	ExportedAt time.Time          `yaml:"exported_at"`
	Providers  []ProviderExport   `yaml:"providers,omitempty"`
	Models     []ModelExport      `yaml:"models,omitempty"`
	ErrorRules []ErrorRuleExport  `yaml:"error_rules,omitempty"`
}

// ProviderExport mirrors model.Provider for serialization. OrgID is deliberately
// omitted (stripped on export, set to NULL on import — target instance owns
// org assignment). APIKey and sensitive ExtraConfig fields are resolved
// (decrypted enc://, env:// preserved) by resolveForExport before marshaling.
type ProviderExport struct {
	Name        string         `yaml:"name"`
	DisplayName string         `yaml:"display_name"`
	AdapterType string         `yaml:"adapter_type"`
	BaseURL     string         `yaml:"base_url"`
	APIKey      string         `yaml:"api_key"`
	ExtraConfig map[string]any `yaml:"extra_config,omitempty"`
	Status      int16          `yaml:"status"`
}

// ModelExport mirrors model.ProviderModel. ProviderName replaces ProviderID so
// the association survives cross-instance import (ids are not stable across DBs).
type ModelExport struct {
	ProviderName    string         `yaml:"provider_name"`
	ModelName       string         `yaml:"model_name"`
	ProviderModel   string         `yaml:"provider_model"`
	Weight          int            `yaml:"weight"`
	Priority        int            `yaml:"priority"`
	InputPrice      float64        `yaml:"input_price"`
	OutputPrice     float64        `yaml:"output_price"`
	Currency        string         `yaml:"currency"`
	RoutingStrategy string         `yaml:"routing_strategy"`
	Status          int16          `yaml:"status"`
}

// ErrorRuleExport mirrors model.ErrorClassificationRule (global, no org_id).
type ErrorRuleExport struct {
	MatchField     string  `yaml:"match_field"`
	Pattern        string  `yaml:"pattern"`
	Classification string  `yaml:"classification"`
	ProviderType   *string `yaml:"provider_type,omitempty"`
	Scope          string  `yaml:"scope"`
	Priority       int     `yaml:"priority"`
	Enabled        bool    `yaml:"enabled"`
}
