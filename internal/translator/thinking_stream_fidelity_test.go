package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 类二 M-C: stream-side thinking fidelity — signature_delta and
// redacted_thinking survive the double translation (anthropic SSE → openai
// chunk → anthropic SSE), and the thinking_delta wrong-key bug (text vs
// thinking) stays fixed.

func streamEvents(t *testing.T, tr *StreamTranslator, chunks []domain.SSEChunk) []domain.SSEEvent {
	t.Helper()
	var out []domain.SSEEvent
	for _, c := range chunks {
		out = append(out, tr.TranslateChunk(c)...)
	}
	return out
}

func TestStreamFidelity_ThinkingTextKey(t *testing.T) {
	t.Parallel()
	// Reverse side: anthropic thinking_delta keys text as "thinking".
	rev := NewReverseStreamTranslator()
	rev.HandleMessageStartForTest(t)
	chunks := rev.TranslateEvent("content_block_delta", []byte(`{"index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`))
	require.Len(t, chunks, 1)
	require.Len(t, chunks[0].Chunk.Choices, 1)
	assert.Equal(t, "step one", chunks[0].Chunk.Choices[0].Delta.ReasoningContent,
		"thinking_delta must read the thinking key (pre-类二 read text — always empty)")
}

func TestStreamFidelity_SignatureDeltaRoundTrip(t *testing.T) {
	t.Parallel()
	rev := NewReverseStreamTranslator()
	rev.HandleMessageStartForTest(t)
	chunks := rev.TranslateEvent("content_block_delta", []byte(`{"index":0,"delta":{"type":"signature_delta","signature":"sig-stream-1"}}`))
	require.Len(t, chunks, 1)
	assert.Equal(t, "sig-stream-1", chunks[0].Chunk.Choices[0].Delta.SignatureDelta)

	// Synthesizer: an active thinking block + signature delta → the client
	// stream receives the signature_delta event.
	syn := NewStreamTranslator("msg_1", "m", 1)
	var all []domain.SSEEvent
	all = append(all, syn.TranslateChunk(domain.SSEChunk{Chunk: chunkWithReasoning("step")})...)
	all = append(all, syn.TranslateChunk(domain.SSEChunk{Chunk: chunkWithSignature("sig-stream-1")})...)

	found := false
	for _, e := range all {
		if e.Event == "content_block_delta" {
			var d struct {
				Delta struct {
					Type      string `json:"type"`
					Signature string `json:"signature"`
				} `json:"delta"`
			}
			require.NoError(t, json.Unmarshal(e.Data, &d))
			if d.Delta.Type == "signature_delta" {
				found = true
				assert.Equal(t, "sig-stream-1", d.Delta.Signature)
			}
		}
	}
	assert.True(t, found, "signature_delta must reach the anthropic client stream")
}

func TestStreamFidelity_RedactedThinkingPassthrough(t *testing.T) {
	t.Parallel()
	rev := NewReverseStreamTranslator()
	rev.HandleMessageStartForTest(t)
	chunks := rev.TranslateEvent("content_block_start", []byte(`{"index":0,"content_block":{"type":"redacted_thinking","data":"enc-1"}}`))
	require.Len(t, chunks, 1)
	require.NotNil(t, chunks[0].Chunk.Choices[0].Delta.RedactedThinking)

	syn := NewStreamTranslator("msg_1", "m", 1)
	events := syn.TranslateChunk(chunks[0])
	require.NotEmpty(t, events)
	var start struct {
		ContentBlock struct {
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"content_block"`
	}
	found := false
	for _, e := range events {
		if e.Event != "content_block_start" {
			continue
		}
		found = true
		require.NoError(t, json.Unmarshal(e.Data, &start))
		assert.Equal(t, "redacted_thinking", start.ContentBlock.Type)
		assert.Equal(t, "enc-1", start.ContentBlock.Data)
	}
	assert.True(t, found, "redacted content_block_start must reach the client stream")
}

// --- helpers -------------------------------------------------------------

func (t *ReverseStreamTranslator) HandleMessageStartForTest(tb testing.TB) {
	tb.Helper()
	t.TranslateEvent("message_start", []byte(`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`))
}

func chunkWithReasoning(s string) *domain.OpenAIChunk {
	return &domain.OpenAIChunk{
		ID: "msg_1", Object: "chat.completion.chunk", Model: "m",
		Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{ReasoningContent: s}}},
	}
}

func chunkWithSignature(s string) *domain.OpenAIChunk {
	return &domain.OpenAIChunk{
		ID: "msg_1", Object: "chat.completion.chunk", Model: "m",
		Choices: []domain.OpenAIChunkChoice{{Index: 0, Delta: domain.OpenAIChunkDelta{SignatureDelta: s}}},
	}
}
