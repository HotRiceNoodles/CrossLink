package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt-cache carry: anthropic-inbound cache_control markers must survive the
// AnthropicToOpenAI → OpenAIToAnthropicRequest round trip, or same-protocol
// upstreams never activate explicit prompt caching.

const markerEphemeral = `{"type":"ephemeral"}`

func roundTripReq(t *testing.T, in *domain.AnthropicRequest) *domain.AnthropicRequest {
	t.Helper()
	oai, err := AnthropicToOpenAI(in, in.Model)
	require.NoError(t, err)
	out, err := OpenAIToAnthropicRequest(oai)
	require.NoError(t, err)
	return out
}

func TestCacheControl_SystemAndToolsVerbatim(t *testing.T) {
	t.Parallel()
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 32,
		System:    json.RawMessage(`[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral","ttl":"1h"}}]`),
		Tools:     json.RawMessage(`[{"name":"t1","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]`),
		Messages:  []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out := roundTripReq(t, in)
	assert.JSONEq(t, string(in.System), string(out.System), "system block array must round-trip verbatim (markers live in the raw bytes)")
	assert.JSONEq(t, string(in.Tools), string(out.Tools), "tools array must round-trip verbatim")
}

func TestCacheControl_MessageTailMarkers(t *testing.T) {
	t.Parallel()
	// Marker on the last block of a user text message (the canonical position).
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 32,
		Messages:  []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"},{"type":"text","text":"world","cache_control":{"type":"ephemeral"}}]`)}},
	}
	out := roundTripReq(t, in)
	var blocks []map[string]any
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 1, "joined text emits one block")
	assert.Equal(t, map[string]any{"type": "ephemeral"}, blocks[0]["cache_control"])
}

func TestCacheControl_ToolUseAndToolResultMarkers(t *testing.T) {
	t.Parallel()
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 32,
		Messages: []domain.AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"calling"},{"type":"tool_use","id":"tu1","name":"f","input":{},"cache_control":{"type":"ephemeral"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu1","content":"ok","cache_control":{"type":"ephemeral"}}]`)},
		},
	}
	out := roundTripReq(t, in)
	// assistant: marker re-attached to the LAST rebuilt block (the tool_use).
	var aBlocks []map[string]any
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &aBlocks))
	last := aBlocks[len(aBlocks)-1]
	assert.Equal(t, "tool_use", last["type"])
	assert.Equal(t, map[string]any{"type": "ephemeral"}, last["cache_control"])

	// tool_result: marker rides its own message.
	var uBlocks []map[string]any
	require.NoError(t, json.Unmarshal(out.Messages[1].Content, &uBlocks))
	require.Len(t, uBlocks, 1)
	assert.Equal(t, map[string]any{"type": "ephemeral"}, uBlocks[0]["cache_control"])
}

func TestCacheControl_StringUserContentUpgradedToBlock(t *testing.T) {
	t.Parallel()
	// A single-block user message (string content in anthropic form after the
	// forward join) carrying a marker must come back as a block array — a
	// bare string cannot carry cache_control.
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 32,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"long stable prefix","cache_control":{"type":"ephemeral"}}]`)},
		},
	}
	out := roundTripReq(t, in)
	var blocks []map[string]any
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 1)
	assert.Equal(t, "long stable prefix", blocks[0]["text"])
	assert.Equal(t, map[string]any{"type": "ephemeral"}, blocks[0]["cache_control"])
}

func TestCacheControl_AbsentMarkersLeavePathUnchanged(t *testing.T) {
	t.Parallel()
	// No markers anywhere: a block-array system (even markerless) now round
	// trips VERBATIM — more faithful than the old join-to-string, and both
	// forms are valid Anthropic wire shapes; user string content stays a
	// plain string (no marker → no block-array upgrade).
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 32,
		System:    json.RawMessage(`[{"type":"text","text":"be brief"}]`),
		Messages:  []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)}},
	}
	out := roundTripReq(t, in)
	assert.JSONEq(t, string(in.System), string(out.System))
	var content string
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &content))
	assert.Equal(t, "hello", content)
}

func TestCacheCreationSurfacesToAnthropicClient(t *testing.T) {
	t.Parallel()
	// Internal → anthropic response: cache_creation_input_tokens must surface
	// (Anthropic bills cache writes at 1.25×; clients need the split).
	resp := &domain.OpenAIResponse{
		Usage: domain.OpenAIUsage{
			PromptTokens:        100,
			CompletionTokens:    10,
			CacheCreationTokens: 40,
			PromptTokensDetails: &domain.PromptTokensDetails{CachedTokens: 60},
		},
	}
	out, err := OpenAIToAnthropic(resp, "m")
	require.NoError(t, err)
	assert.Equal(t, 60, out.Usage.CacheReadInputTokens)
	assert.Equal(t, 40, out.Usage.CacheCreationInputTokens)
}
