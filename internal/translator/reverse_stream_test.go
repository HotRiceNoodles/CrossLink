package translator

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newReverseStream() *ReverseStreamTranslator {
	return NewReverseStreamTranslator()
}

func TestReverseStream_MessageStart(t *testing.T) {
	tl := newReverseStream()
	data, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    "msg_abc",
			"model": "claude-sonnet-4-20250514",
			"usage": map[string]any{"input_tokens": 10},
		},
	})
	chunks := tl.TranslateEvent("message_start", data)
	assert.Len(t, chunks, 1)
	assert.NotNil(t, chunks[0].Chunk)
	assert.Equal(t, "msg_abc", chunks[0].Chunk.ID)
	assert.Equal(t, "assistant", chunks[0].Chunk.Choices[0].Delta.Role)
}

func TestReverseStream_TextDelta(t *testing.T) {
	tl := newReverseStream()

	// message_start first
	startData, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_1", "model": "claude", "usage": map[string]any{"input_tokens": 5},
		},
	})
	tl.TranslateEvent("message_start", startData)

	// text delta
	deltaData, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "Hello"},
	})
	chunks := tl.TranslateEvent("content_block_delta", deltaData)
	assert.Len(t, chunks, 1)
	assert.Equal(t, "Hello", chunks[0].Chunk.Choices[0].Delta.Content)
}

func TestReverseStream_ToolUse(t *testing.T) {
	tl := newReverseStream()

	// message_start
	startData, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_1", "model": "claude", "usage": map[string]any{"input_tokens": 5},
		},
	})
	tl.TranslateEvent("message_start", startData)

	// tool_use content_block_start
	blockStart, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather"},
	})
	chunks := tl.TranslateEvent("content_block_start", blockStart)
	assert.Len(t, chunks, 1)
	assert.Len(t, chunks[0].Chunk.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "toolu_1", chunks[0].Chunk.Choices[0].Delta.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", chunks[0].Chunk.Choices[0].Delta.ToolCalls[0].Function.Name)

	// input_json_delta
	argDelta, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city":`},
	})
	chunks = tl.TranslateEvent("content_block_delta", argDelta)
	assert.Len(t, chunks, 1)
	assert.Equal(t, `{"city":`, chunks[0].Chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments)
}

func TestReverseStream_MessageStop(t *testing.T) {
	tl := newReverseStream()
	chunks := tl.TranslateEvent("message_stop", []byte(`{"type":"message_stop"}`))
	assert.Len(t, chunks, 1)
	assert.True(t, chunks[0].Done)
}

func TestReverseStream_MessageDelta_FinishReason(t *testing.T) {
	tl := newReverseStream()

	// message_start first to set state
	startData, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_1", "model": "claude", "usage": map[string]any{"input_tokens": 5},
		},
	})
	tl.TranslateEvent("message_start", startData)

	deltaData, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 20},
	})
	chunks := tl.TranslateEvent("message_delta", deltaData)
	assert.Len(t, chunks, 1)
	assert.NotNil(t, chunks[0].Chunk.Choices[0].FinishReason)
	assert.Equal(t, "stop", *chunks[0].Chunk.Choices[0].FinishReason)
	assert.Equal(t, 20, chunks[0].Chunk.Usage.CompletionTokens)
}
