package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/assert"
)

func strPtr(s string) *string { return &s }

func chunk(data string) domain.SSEChunk {
	var c domain.OpenAIChunk
	_ = json.Unmarshal([]byte(data), &c)
	return domain.SSEChunk{Chunk: &c}
}

func doneChunk() domain.SSEChunk {
	return domain.SSEChunk{Done: true}
}

func eventTypes(events []domain.SSEEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Event
	}
	return types
}

func TestStreamTranslator_BasicStream(t *testing.T) {
	st := NewStreamTranslator("msg_test123", "claude-sonnet-4-20250514", 0)

	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"message_start"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_start", "content_block_delta"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_delta"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	assert.Equal(t, []string{"content_block_stop", "message_delta", "message_stop"}, eventTypes(events))

	events = st.TranslateChunk(doneChunk())
	assert.Nil(t, events)
}

func TestStreamTranslator_EmptyResponse(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"message_start"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	assert.Equal(t, []string{"message_delta", "message_stop"}, eventTypes(events))
}

func TestStreamTranslator_ContentFilter(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"some text"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_start", "content_block_delta"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`))
	assert.Equal(t, []string{"content_block_stop", "message_delta", "message_stop"}, eventTypes(events))

	var msgDelta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(events[1].Data, &msgDelta)
	assert.Equal(t, "end_turn", msgDelta.Delta.StopReason)
}

func TestStreamTranslator_EmptyDeltaIgnored(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_start", "content_block_delta"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`))
	assert.Empty(t, events)
}

func TestStreamTranslator_OnProviderDisconnect_BlockActive(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`))

	events := st.OnProviderDisconnect()
	assert.Equal(t, []string{"content_block_stop", "message_delta", "message_stop"}, eventTypes(events))
}

func TestStreamTranslator_OnProviderDisconnect_Started(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))

	events := st.OnProviderDisconnect()
	assert.Equal(t, []string{"message_delta", "message_stop"}, eventTypes(events))
}

func TestStreamTranslator_OnProviderDisconnect_Init(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	events := st.OnProviderDisconnect()
	assert.Equal(t, []string{"message_start", "message_delta", "message_stop"}, eventTypes(events))
}

func TestStreamTranslator_UsageOnlyChunk(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`))

	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}}`))
	assert.Empty(t, events)
	assert.Equal(t, 50, st.inputTokens)
	assert.Equal(t, 5, st.outputTokens)
}

func TestStreamTranslator_FinishReasonLength(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"cut"},"finish_reason":"length"}]}`))

	assert.Equal(t, []string{"content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}, eventTypes(events))

	var msgDelta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(events[3].Data, &msgDelta)
	assert.Equal(t, "max_tokens", msgDelta.Delta.StopReason)
}

func TestStreamTranslator_NilChunk(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)
	events := st.TranslateChunk(domain.SSEChunk{Chunk: nil})
	assert.Nil(t, events)
}

func TestStreamTranslator_DoneChunk(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)
	events := st.TranslateChunk(doneChunk())
	assert.Nil(t, events)
}

// --- Tool Use streaming tests ---

func TestStreamTranslator_ToolCallStreaming(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	// First chunk: message_start
	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	assert.Equal(t, []string{"message_start"}, eventTypes(events))

	// Tool call start with ID + name
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_start"}, eventTypes(events))
	assert.Equal(t, "content_block_start", events[0].Event)

	// Parse content_block_start data
	var startData struct {
		Index       int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	_ = json.Unmarshal(events[0].Data, &startData)
	assert.Equal(t, "tool_use", startData.ContentBlock.Type)
	assert.Equal(t, "call_abc", startData.ContentBlock.ID)
	assert.Equal(t, "get_weather", startData.ContentBlock.Name)

	// Arguments delta
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_delta"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Beijing\"}"}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_delta"}, eventTypes(events))

	// Finish with tool_calls
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	assert.Equal(t, []string{"content_block_stop", "message_delta", "message_stop"}, eventTypes(events))

	var msgDelta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(events[1].Data, &msgDelta)
	assert.Equal(t, "tool_use", msgDelta.Delta.StopReason)
}

func TestStreamTranslator_MultipleToolCallsStreaming(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	// message_start
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))

	// First tool call
	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_start"}, eventTypes(events))

	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_delta"}, eventTypes(events))

	// Second tool call starts — first should auto-close
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_stop", "content_block_start"}, eventTypes(events))

	// Second tool args
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"b.go\"}"}}]},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_delta"}, eventTypes(events))

	// Finish
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	assert.Equal(t, []string{"content_block_stop", "message_delta", "message_stop"}, eventTypes(events))
}

func TestStreamTranslator_TextThenToolCalls(t *testing.T) {
	st := NewStreamTranslator("msg_test", "claude-sonnet-4-20250514", 0)

	// message_start
	st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))

	// Text content
	events := st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Let me check."},"finish_reason":null}]}`))
	assert.Equal(t, []string{"content_block_start", "content_block_delta"}, eventTypes(events))

	// Text block closes automatically when tool_call starts
	events = st.TranslateChunk(chunk(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":""}}]},"finish_reason":null}]}`))
	// Should close text block (content_block_stop), then start tool_use block (content_block_start)
	assert.Equal(t, []string{"content_block_stop", "content_block_start"}, eventTypes(events))
}
