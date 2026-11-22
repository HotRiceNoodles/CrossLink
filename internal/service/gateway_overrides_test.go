package service

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/admission"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M3.2/P3: the model-level extension controls — registered dialect params
// ride the envelope at admission, routes gate/override them at execution, and
// the openai-compatible serialization merges them onto the wire.

func TestAdmit_RegisteredDialectParamCarried(t *testing.T) {
	t.Parallel()
	env, err := admission.Admit("openai", []byte(`{"model":"qwen3","messages":[],"enable_thinking":true}`), "")
	require.NoError(t, err)
	require.NotNil(t, env, "registered dialect param must be carried, not dropped")
	require.Equal(t, json.RawMessage(`true`), env.Params["dialect.enable_thinking"])

	// Wrong type is rejected with the param named.
	_, err = admission.Admit("openai", []byte(`{"model":"q","enable_thinking":"yes"}`), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enable_thinking")

	// Wire-name mapping sanity for the serialization side.
	wire, ok := admission.DialectWireName("dialect.enable_thinking")
	require.True(t, ok)
	assert.Equal(t, "enable_thinking", wire)
	_, ok = admission.DialectWireName("dialect.not_registered")
	assert.False(t, ok)
}

func TestValidateModelExtensionConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{"empty", ``, false},
		{"unrelated keys", `{"fallback_models":["x"]}`, false},
		{"valid overrides+allow", `{"request_overrides":{"temperature":0.3,"enable_thinking":true},"allowed_extensions":["enable_thinking"]}`, false},
		{"deny-list key rejected", `{"request_overrides":{"max_tokens":4096}}`, true},
		{"messages key rejected", `{"request_overrides":{"messages":[]}}`, true},
		{"unknown key rejected", `{"request_overrides":{"service_tier":"auto"}}`, true},
		{"wrong type rejected", `{"request_overrides":{"temperature":true}}`, true},
		{"unregistered extension rejected", `{"allowed_extensions":["speed"]}`, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := router.ValidateModelExtensionConfig(json.RawMessage(tc.extra))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateModelExtensionConfig(%q) error = %v, wantErr %v", tc.extra, err, tc.wantErr)
			}
		})
	}
}

func TestApplyRouteRequestOverrides(t *testing.T) {
	t.Parallel()

	t.Run("admin override wins over client value", func(t *testing.T) {
		t.Parallel()
		req := &domain.OpenAIRequest{Temperature: f64(0.9)}
		route := &router.RouteResult{RequestOverrides: map[string]any{"temperature": 0.2}}
		applyRouteRequestOverrides(req, route)
		require.NotNil(t, req.Temperature)
		assert.Equal(t, 0.2, *req.Temperature)
	})

	t.Run("client dialect param stripped without opt-in", func(t *testing.T) {
		t.Parallel()
		req := &domain.OpenAIRequest{Extensions: &domain.ProtocolExtensions{
			Params: map[string]json.RawMessage{"dialect.enable_thinking": json.RawMessage(`true`)},
		}}
		applyRouteRequestOverrides(req, &router.RouteResult{})
		assert.Empty(t, req.Extensions.Params, "no opt-in → dialect param must not ride to the upstream")
	})

	t.Run("allowed_extensions forwards client param", func(t *testing.T) {
		t.Parallel()
		req := &domain.OpenAIRequest{Extensions: &domain.ProtocolExtensions{
			Params: map[string]json.RawMessage{"dialect.enable_thinking": json.RawMessage(`false`)},
		}}
		applyRouteRequestOverrides(req, &router.RouteResult{AllowedExtensions: []string{"enable_thinking"}})
		assert.Equal(t, json.RawMessage(`false`), req.Extensions.Params["dialect.enable_thinking"])
	})

	t.Run("override implies opt-in and injects", func(t *testing.T) {
		t.Parallel()
		req := &domain.OpenAIRequest{}
		applyRouteRequestOverrides(req, &router.RouteResult{RequestOverrides: map[string]any{"enable_thinking": true}})
		require.NotNil(t, req.Extensions)
		assert.Equal(t, json.RawMessage(`true`), req.Extensions.Params["dialect.enable_thinking"])
	})
}
