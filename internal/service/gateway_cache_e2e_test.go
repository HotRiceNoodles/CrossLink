package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newJSONServer is a minimal upstream answering one static JSON body.
func newJSONServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Cache-fidelity END-TO-END acceptance: through the REAL chain (translate →
// gateway attempt loop → AnthropicProvider → httptest upstream capturing the
// actual wire bytes), proving (A) cache_control markers reach the upstream
// wire and (B) cache usage flows back to the client response and the
// usage-log source fields.

func newCacheTestEnv(t *testing.T) (*GatewayService, *capturedRequest) {
	t.Helper()
	srv, cr, _ := anthropicUpstream(t, http.StatusOK)
	reg := provider.NewRegistry()
	anth, err := provider.NewAnthropicProvider("anCache", srv.URL, "k", []byte(`{"api_version":"2023-06-01"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("anCache", anth)
	resolver := matrixResolver(reg, model.ProviderModel{
		ID: 81, ModelName: "mcache", ProviderModel: "an-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "anCache", Status: 1, ID: 81},
	})
	return NewGatewayService(resolver, reg, nil, nil, nil), cr
}

func TestCacheE2E_MarkersReachUpstreamWire(t *testing.T) {
	t.Parallel()
	svc, cr := newCacheTestEnv(t)

	// Client request with markers in the canonical positions: system block,
	// tools, and the last message block.
	req := &domain.AnthropicRequest{
		Model:     "mcache",
		MaxTokens: 32,
		System:    json.RawMessage(`[{"type":"text","text":"You are terse.","cache_control":{"type":"ephemeral"}}]`),
		Tools:     json.RawMessage(`[{"name":"f1","description":"do it","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"stable prefix"},{"type":"text","text":"turn 1","cache_control":{"type":"ephemeral"}}]`)},
		},
	}
	_, err := svc.Chat(context.Background(), req, "", 0)
	require.NoError(t, err)

	wire := cr.last()
	require.NotNil(t, wire, "upstream must have been called")

	// System: the ORIGINAL block array with its marker, verbatim on the wire.
	sys, ok := wire["system"].([]any)
	require.True(t, ok, "system must be a block array on the wire, got %T", wire["system"])
	require.Len(t, sys, 1)
	sysBlock := sys[0].(map[string]any)
	assert.Equal(t, map[string]any{"type": "ephemeral"}, sysBlock["cache_control"])
	assert.Equal(t, "You are terse.", sysBlock["text"])

	// Tools: original array with per-tool marker.
	tools, ok := wire["tools"].([]any)
	require.True(t, ok, "tools must be an array on the wire")
	require.Len(t, tools, 1)
	assert.Equal(t, map[string]any{"type": "ephemeral"}, tools[0].(map[string]any)["cache_control"])

	// Last message block carries the marker.
	msgs, ok := wire["messages"].([]any)
	require.True(t, ok, "messages must be an array")
	lastMsg := msgs[len(msgs)-1].(map[string]any)
	content, ok := lastMsg["content"].([]any)
	require.True(t, ok, "marked user content must be a block array, got %T", lastMsg["content"])
	last := content[len(content)-1].(map[string]any)
	assert.Equal(t, map[string]any{"type": "ephemeral"}, last["cache_control"])
}

func TestCacheE2E_NoMarkersStayClean(t *testing.T) {
	t.Parallel()
	svc, cr := newCacheTestEnv(t)

	_, err := svc.Chat(context.Background(), &domain.AnthropicRequest{
		Model: "mcache", MaxTokens: 32,
		Messages: []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"plain"}]`)}},
	}, "", 0)
	require.NoError(t, err)

	wire := cr.last()
	assert.NotContains(t, wire, "cache_control", "no markers in → no markers out")
	if content, ok := wire["messages"].([]any); ok {
		lastMsg := content[len(content)-1].(map[string]any)
		c, isArr := lastMsg["content"].([]any)
		if isArr {
			for _, b := range c {
				assert.NotContains(t, b.(map[string]any), "cache_control")
			}
		}
	}
}

// The upstream fixture answers with a static body; this test swaps in an
// upstream that reports cache usage, and asserts the full return path:
// client-facing anthropic usage AND the usage-log source (ChatResult fields).
func TestCacheE2E_UsageFlowsBack(t *testing.T) {
	t.Parallel()
	srv := newJSONServer(t, `{"id":"msg_c","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":700,"cache_creation_input_tokens":200}}`)
	reg := provider.NewRegistry()
	anth, err := provider.NewAnthropicProvider("anCache2", srv, "k", []byte(`{"api_version":"2023-06-01"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	reg.Register("anCache2", anth)
	resolver := matrixResolver(reg, model.ProviderModel{
		ID: 82, ModelName: "mcache2", ProviderModel: "an-model", Weight: 1, Status: 1,
		Provider: model.Provider{Name: "anCache2", Status: 1, ID: 82},
	})
	svc := NewGatewayService(resolver, reg, nil, nil, nil)

	res, err := svc.Chat(context.Background(), &domain.AnthropicRequest{
		Model: "mcache2", MaxTokens: 32,
		Messages: []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}, "", 0)
	require.NoError(t, err)

	// Client-facing anthropic usage: full cache split visible.
	assert.Equal(t, 700, res.Response.Usage.CacheReadInputTokens)
	assert.Equal(t, 200, res.Response.Usage.CacheCreationInputTokens)

	// Usage-log source fields (what handlers write to usage_logs).
	assert.Equal(t, 700, res.CacheReadTokens)
	assert.Equal(t, 200, res.CacheCreationTokens)
	// Anthropic semantics: InputTokens excludes cache; the gateway folds
	// read+creation into the billed prompt total (10+700+200=910).
	assert.Equal(t, 910, res.InputTokens)
}
