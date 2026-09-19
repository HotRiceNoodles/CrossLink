package translator

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReverseStreamTranslator_CacheReadInputTokens(t *testing.T) {
	tl := newReverseStream()

	// message_start with cache tokens — OpenAI semantics: prompt_tokens
	// includes cached tokens (100 + 45 read + 10 write)
	startData, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    "msg_cache_test",
			"model": "claude-sonnet-4-20250514",
			"usage": map[string]any{
				"input_tokens":               100,
				"cache_read_input_tokens":    45,
				"cache_creation_input_tokens": 10,
			},
		},
	})
	chunks := tl.TranslateEvent("message_start", startData)
	assert.Len(t, chunks, 1)
	assert.NotNil(t, chunks[0].Chunk)
	assert.Equal(t, 155, chunks[0].Chunk.Usage.PromptTokens)

	// message_delta should propagate cache via PromptTokensDetails and carry
	// cache_creation for billing
	deltaData, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 30},
	})
	chunks = tl.TranslateEvent("message_delta", deltaData)
	assert.Len(t, chunks, 1)
	assert.NotNil(t, chunks[0].Chunk.Usage)
	assert.NotNil(t, chunks[0].Chunk.Usage.PromptTokensDetails)
	assert.Equal(t, 45, chunks[0].Chunk.Usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 10, chunks[0].Chunk.Usage.CacheCreationTokens)
	assert.Equal(t, 155, chunks[0].Chunk.Usage.PromptTokens)
}

func TestReverseStreamTranslator_NoCacheToken(t *testing.T) {
	tl := newReverseStream()

	// message_start WITHOUT cache_read_input_tokens
	startData, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    "msg_no_cache",
			"model": "claude-sonnet-4-20250514",
			"usage": map[string]any{
				"input_tokens": 50,
			},
		},
	})
	tl.TranslateEvent("message_start", startData)

	// message_delta should have nil PromptTokensDetails (no cache)
	deltaData, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 10},
	})
	chunks := tl.TranslateEvent("message_delta", deltaData)
	assert.Len(t, chunks, 1)
	assert.Nil(t, chunks[0].Chunk.Usage.PromptTokensDetails)
}
