package translator

import (
	"encoding/json"

	"github.com/crosslink/internal/domain"
)

func buildMessageStart(msgID, model string, inputTokens int) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  inputTokens,
				"output_tokens": 0,
			},
		},
	})
	return domain.SSEEvent{Event: "message_start", Data: data}
}

func contentBlockStart(index int) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	return domain.SSEEvent{Event: "content_block_start", Data: data}
}

func thinkingBlockStart(index int) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type": "thinking",
			"thinking": "",
		},
	})
	return domain.SSEEvent{Event: "content_block_start", Data: data}
}

func thinkingBlockDelta(index int, text string) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": text,
		},
	})
	return domain.SSEEvent{Event: "content_block_delta", Data: data}
}

func toolUseBlockStart(index int, id, name string) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
	return domain.SSEEvent{Event: "content_block_start", Data: data}
}

func toolUseArgumentsDelta(index int, argsChunk string) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type":          "input_json_delta",
			"partial_json":  argsChunk,
		},
	})
	return domain.SSEEvent{Event: "content_block_delta", Data: data}
}

func contentBlockDelta(index int, text string) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
	return domain.SSEEvent{Event: "content_block_delta", Data: data}
}

func contentBlockStop(index int) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": index,
	})
	return domain.SSEEvent{Event: "content_block_stop", Data: data}
}

func messageDelta(stopReason string, outputTokens int) domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": outputTokens,
		},
	})
	return domain.SSEEvent{Event: "message_delta", Data: data}
}

func messageStop() domain.SSEEvent {
	data, _ := json.Marshal(map[string]any{
		"type": "message_stop",
	})
	return domain.SSEEvent{Event: "message_stop", Data: data}
}
