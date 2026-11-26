package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase-1 effort wire tests (2026-11-25 checklist §6/§8): outbound bodies must
// carry output_config on supporting anthropic versions, strip it (audited) on
// legacy versions, and translate effort→reasoning_effort on openai upstreams.

func effortEnvelope(effort string) *domain.ProtocolExtensions {
	return &domain.ProtocolExtensions{
		SourceProtocol: "anthropic",
		Params:         map[string]json.RawMessage{"anthropic.output_config": json.RawMessage(`{"effort":"` + effort + `"}`)},
	}
}

func anthropicOKBody(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
		"content":     []any{map[string]any{"type": "text", "text": "ok"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
}

func TestAnthropicEffort_MergedOnWire(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		_ = json.Unmarshal(raw, &gotBody)
		anthropicOKBody(w)
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("an", server.URL, "k", []byte(`{"api_version":"2026-10-21"}`), 10*time.Second)
	require.NoError(t, err)
	req := &domain.OpenAIRequest{Model: "m", MaxTokens: intPtr(8), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req.Extensions = effortEnvelope("xhigh")

	_, err = p.Chat(context.Background(), req, "k")
	require.NoError(t, err)

	oc, ok := gotBody["output_config"].(map[string]any)
	require.True(t, ok, "outbound body must carry output_config, got %v", gotBody)
	assert.Equal(t, "xhigh", oc["effort"])
}

func TestAnthropicEffort_StrippedOnUnsupportedVersion(t *testing.T) {
	t.Parallel()
	var bodyText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		n, _ := r.Body.Read(raw)
		bodyText = string(raw[:n])
		anthropicOKBody(w)
	}))
	defer server.Close()

	// Default version 2023-06-01 has no EffortSupport in the caps table.
	p, err := NewAnthropicProvider("an", server.URL, "k", nil, 10*time.Second)
	require.NoError(t, err)
	req := &domain.OpenAIRequest{Model: "m", MaxTokens: intPtr(8), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req.Extensions = effortEnvelope("high")

	var strips []upstream.ParamStrip
	ctx := upstream.WithStripSink(context.Background(), &strips)
	_, err = p.Chat(ctx, req, "k")
	require.NoError(t, err)

	assert.False(t, strings.Contains(bodyText, "output_config"), "legacy version must not receive output_config")
	require.Len(t, strips, 1, "strip must be audited")
	assert.Equal(t, "output_config", strips[0].Param)
	assert.Equal(t, "version_without_effort_support", strips[0].Reason)
}

func TestOpenAICompatible_EffortTranslatedToReasoningEffort(t *testing.T) {
	t.Parallel()
	req := &domain.OpenAIRequest{Model: "m", MaxTokens: intPtr(8), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req.Extensions = effortEnvelope("medium")

	var top map[string]any
	require.NoError(t, json.Unmarshal(marshalChatBodyWithExtensions(req), &top))
	assert.Equal(t, "medium", top["reasoning_effort"], "effort must translate to reasoning_effort")
	_, hasOC := top["output_config"]
	assert.False(t, hasOC, "raw output_config object must NOT leak onto the openai wire")

	// Identity path regression: enable_thinking keeps merging verbatim.
	req2 := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req2.Extensions = &domain.ProtocolExtensions{SourceProtocol: "openai", Params: map[string]json.RawMessage{
		"dialect.enable_thinking": json.RawMessage(`true`),
	}}
	var top2 map[string]any
	require.NoError(t, json.Unmarshal(marshalChatBodyWithExtensions(req2), &top2))
	assert.Equal(t, true, top2["enable_thinking"])
}

// Phase-2 format: output_config.format shares openai's response_format wire
// shape — the translation is a key rename; both subfields can coexist.
func TestOpenAICompatible_FormatTranslatedToResponseFormat(t *testing.T) {
	t.Parallel()
	req := &domain.OpenAIRequest{Model: "m", Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req.Extensions = &domain.ProtocolExtensions{
		SourceProtocol: "anthropic",
		Params: map[string]json.RawMessage{"anthropic.output_config": json.RawMessage(
			`{"effort":"high","format":{"type":"json_schema","json_schema":{"name":"out","strict":true}}}`)},
	}
	var top map[string]any
	require.NoError(t, json.Unmarshal(marshalChatBodyWithExtensions(req), &top))
	assert.Equal(t, "high", top["reasoning_effort"])
	rf, ok := top["response_format"].(map[string]any)
	require.True(t, ok, "response_format must be on the wire, got %v", top)
	assert.Equal(t, "json_schema", rf["type"])
	_, hasOC := top["output_config"]
	assert.False(t, hasOC, "raw output_config object must NOT leak onto the openai wire")
}

// Phase-2 §4 outbound half: the admission-whitelisted anthropic-beta header
// reaches the upstream (the envelope carry was dead without setBetaHeader).
func TestAnthropicBetaHeader_Forwarded(t *testing.T) {
	t.Parallel()
	var gotBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		anthropicOKBody(w)
	}))
	defer server.Close()

	p, err := NewAnthropicProvider("an", server.URL, "k", nil, 10*time.Second)
	require.NoError(t, err)
	req := &domain.OpenAIRequest{Model: "m", MaxTokens: intPtr(8), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	req.Extensions = &domain.ProtocolExtensions{
		SourceProtocol: "anthropic",
		Headers:        map[string]string{"anthropic-beta": "thinking-display-updates-2026-08-18"},
	}
	_, err = p.Chat(context.Background(), req, "k")
	require.NoError(t, err)
	assert.Equal(t, "thinking-display-updates-2026-08-18", gotBeta)

	// No envelope header → no outbound header.
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-beta") != "" {
			t.Error("beta header must not be set without an envelope carry")
		}
		anthropicOKBody(w)
	}))
	defer server2.Close()
	p2, _ := NewAnthropicProvider("an2", server2.URL, "k", nil, 10*time.Second)
	req2 := &domain.OpenAIRequest{Model: "m", MaxTokens: intPtr(8), Messages: []domain.OpenAIMessage{{Role: "user", Content: "hi"}}}
	_, err = p2.Chat(context.Background(), req2, "k")
	require.NoError(t, err)
}
