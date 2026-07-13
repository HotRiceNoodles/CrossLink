package configio

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestYAMLRoundTrip verifies the full serialize→deserialize path that the CLI
// mains rely on (yaml.Marshal in config-export, yaml.Unmarshal in config-import).
// This closes the v2 review Major #8: confirm yaml.v3 correctly round-trips the
// ExportBundle, especially map[string]any ExtraConfig and *string ProviderType
// (json.RawMessage would have been base64-encoded; map[string]any must not be).
func TestYAMLRoundTrip(t *testing.T) {
	providerType := "openai_compatible"
	bundle := &ExportBundle{
		Version:    BundleVersion,
		ExportedAt: time.Date(2026, 7, 12, 10, 30, 0, 0, time.UTC),
		Providers: []ProviderExport{
			{
				Name: "openai", DisplayName: "OpenAI", AdapterType: "openai_compatible",
				BaseURL: "https://api.openai.com/v1", APIKey: "sk-test",
				ExtraConfig: map[string]any{
					"api_protocol":  "openai",
					"access_key_id": "AKIATEST", // sensitive field, plaintext in export
					"nested":        map[string]any{"port": 8080.0, "enabled": true},
				},
				Status: 1,
			},
		},
		Models: []ModelExport{
			{ProviderName: "openai", ModelName: "gpt-4o", ProviderModel: "gpt-4o",
				Weight: 1, Priority: 1, InputPrice: 5.0, OutputPrice: 15.0,
				Currency: "USD", RoutingStrategy: "weighted_random", Status: 1},
		},
		ErrorRules: []ErrorRuleExport{
			{MatchField: "status", Pattern: "429", Classification: "quota",
				ProviderType: &providerType, Scope: "account", Priority: 100, Enabled: true},
			{MatchField: "code", Pattern: "insufficient_quota", Classification: "quota",
				ProviderType: nil, Scope: "model", Priority: 50, Enabled: true},
		},
	}

	yamlBytes, err := yaml.Marshal(bundle)
	require.NoError(t, err, "marshal must succeed for the canonical bundle shape")

	// Sanity: the marshaled YAML must NOT base64-encode the ExtraConfig map.
	// (If it did, "api_protocol" would not appear as a literal key.)
	assert.Contains(t, string(yamlBytes), "api_protocol:", "ExtraConfig map must serialize as literal YAML, not base64")
	assert.Contains(t, string(yamlBytes), "access_key_id:", "sensitive extra_config key must be a literal field")
	assert.Contains(t, string(yamlBytes), "sk-test", "api_key plaintext must be present")

	var decoded ExportBundle
	require.NoError(t, yaml.Unmarshal(yamlBytes, &decoded), "unmarshal must succeed")

	// Core fields survive.
	assert.Equal(t, bundle.Version, decoded.Version)
	assert.Equal(t, bundle.ExportedAt.UTC().Format(time.RFC3339), decoded.ExportedAt.UTC().Format(time.RFC3339))
	require.Len(t, decoded.Providers, 1)
	require.Len(t, decoded.Models, 1)
	require.Len(t, decoded.ErrorRules, 2)

	p := decoded.Providers[0]
	assert.Equal(t, "openai", p.Name)
	assert.Equal(t, "sk-test", p.APIKey)
	assert.Equal(t, "openai", p.ExtraConfig["api_protocol"], "non-sensitive string field round-trips")
	assert.Equal(t, "AKIATEST", p.ExtraConfig["access_key_id"], "sensitive string field round-trips as plaintext")

	// Nested map + numeric/bool coercion.
	nested, ok := p.ExtraConfig["nested"].(map[string]any)
	require.True(t, ok, "nested map must survive as map[string]any (not map[interface{}]interface{})")
	assert.Equal(t, 8080, int(nested["port"].(int)), "numeric nested value survives")
	assert.Equal(t, true, nested["enabled"], "bool nested value survives")

	// *string ProviderType round-trips for both set and nil cases.
	require.NotNil(t, decoded.ErrorRules[0].ProviderType, "non-nil *string ProviderType must survive")
	assert.Equal(t, "openai_compatible", *decoded.ErrorRules[0].ProviderType)
	assert.Nil(t, decoded.ErrorRules[1].ProviderType, "nil *string ProviderType must stay nil")

	// Pricing precision.
	assert.Equal(t, 5.0, decoded.Models[0].InputPrice)
	assert.Equal(t, 15.0, decoded.Models[0].OutputPrice)

	// The decoded bundle must be directly applicable (no further transformation needed).
	// This proves the CLI's unmarshal→ApplyBundle handoff is type-safe.
	assert.Equal(t, "weighted_random", decoded.Models[0].RoutingStrategy)
	assert.Equal(t, int16(1), decoded.Providers[0].Status)
}
