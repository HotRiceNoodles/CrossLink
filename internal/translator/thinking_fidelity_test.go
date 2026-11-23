package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 类二 acceptance: thinking/redacted blocks round trip with signature/data
// VALUES intact (the prefix-check-compatible fidelity level), both on the
// request side (client → upstream) and the response side (upstream → client).

func TestThinking_RequestSideCarryAndReplay(t *testing.T) {
	t.Parallel()
	sig := "sig-abc123"
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"do the thing"`)},
			// Assistant turn WITH thinking+signature and a tool call — the
			// shape the API requires clients to echo back.
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"thinking","thinking":"step 1: parse","signature":"` + sig + `"},
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"tu9","name":"f","input":{"a":1}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu9","content":"done"}]`)},
		},
	}
	oai, err := AnthropicToOpenAI(in, "m")
	require.NoError(t, err)

	// The assistant openai message carries the thinking block VALUES.
	var asst *domain.OpenAIMessage
	for i := range oai.Messages {
		if oai.Messages[i].Role == "assistant" {
			asst = &oai.Messages[i]
		}
	}
	require.NotNil(t, asst, "assistant message must exist")
	require.Len(t, asst.ThinkingBlocks, 1)
	assert.Equal(t, "step 1: parse", asst.ThinkingBlocks[0].Thinking)
	assert.Equal(t, sig, asst.ThinkingBlocks[0].Signature, "signature must survive the forward translation")

	// Reverse: thinking block replays VERBATIM ahead of text/tool_use.
	out, err := OpenAIToAnthropicRequest(oai)
	require.NoError(t, err)
	require.Len(t, out.Messages, 3)
	require.Equal(t, "assistant", out.Messages[1].Role)
	var blocks []domain.ContentBlock
	require.NoError(t, json.Unmarshal(out.Messages[1].Content, &blocks))
	require.Len(t, blocks, 3)
	assert.Equal(t, "thinking", blocks[0].Type)
	assert.Equal(t, "step 1: parse", blocks[0].Thinking)
	assert.Equal(t, sig, blocks[0].Signature, "signature must survive the round trip")
	assert.Equal(t, "text", blocks[1].Type)
	assert.Equal(t, "tool_use", blocks[2].Type)
}

func TestThinking_RedactedDataPreserved(t *testing.T) {
	t.Parallel()
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []domain.AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"redacted_thinking","data":"enc-payload-9f"},{"type":"text","text":"answer"}]`)},
		},
	}
	out := roundTripReq(t, in)
	var blocks []domain.ContentBlock
	require.NoError(t, json.Unmarshal(out.Messages[0].Content, &blocks))
	require.Len(t, blocks, 2)
	assert.Equal(t, "redacted_thinking", blocks[0].Type)
	assert.Equal(t, "enc-payload-9f", blocks[0].Data, "redacted data must round-trip")
	assert.Equal(t, "answer", blocks[1].Text)
}

func TestThinking_ResponseSideCarryAndReplay(t *testing.T) {
	t.Parallel()
	// Upstream returns thinking WITH signature (non-stream) → internal →
	// anthropic client sees the block with signature intact.
	upstream := &domain.AnthropicResponse{
		ID:   "msg_1",
		Type: "message",
		Role: "assistant",
		Content: []domain.ContentBlock{
			{Type: "thinking", Thinking: "reasoned hard", Signature: "sig-xyz"},
			{Type: "text", Text: "answer"},
		},
		StopReason: "end_turn",
		Usage:      domain.AnthropicUsage{InputTokens: 10, OutputTokens: 4},
	}
	internal, err := AnthropicToOpenAIResponse(upstream)
	require.NoError(t, err)
	require.Len(t, internal.ThinkingBlocks, 1, "response-side carry must capture the block")
	assert.Equal(t, "sig-xyz", internal.ThinkingBlocks[0].Signature)

	client, err := OpenAIToAnthropic(internal, "m")
	require.NoError(t, err)
	require.NotEmpty(t, client.Content)
	assert.Equal(t, "thinking", client.Content[0].Type)
	assert.Equal(t, "reasoned hard", client.Content[0].Thinking)
	assert.Equal(t, "sig-xyz", client.Content[0].Signature, "client-facing thinking block must carry the signature")
	assert.Equal(t, "answer", client.Content[1].Text)
}

func TestThinking_NoBlocksLegacyBehavior(t *testing.T) {
	t.Parallel()
	// No thinking anywhere: request round trip and response assembly behave
	// exactly as before (no synthetic signature-less blocks leaking).
	in := &domain.AnthropicRequest{
		Model:     "m",
		MaxTokens: 32,
		Messages:  []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out := roundTripReq(t, in)
	for _, m := range out.Messages {
		// The carry lives on the internal OpenAI message, not the wire form —
		// absence on the wire struct is the no-leak guarantee itself.
		var blocks []domain.ContentBlock
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				assert.NotEqual(t, "thinking", b.Type)
				assert.NotEqual(t, "redacted_thinking", b.Type)
			}
		}
	}

	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{Message: domain.OpenAIMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		Usage:   domain.OpenAIUsage{PromptTokens: 1, CompletionTokens: 1},
	}
	client, err := OpenAIToAnthropic(resp, "m")
	require.NoError(t, err)
	assert.Equal(t, "text", client.Content[0].Type)
}
