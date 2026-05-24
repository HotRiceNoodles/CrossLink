package translator

import (
	"encoding/json"

	"github.com/crosslink/internal/domain"
)

// ReverseStreamTranslator converts Anthropic SSE events into OpenAI SSE chunks.
type ReverseStreamTranslator struct {
	messageID    string
	model        string
	inputTokens  int
	outputTokens int
	activeToolID string
}

func NewReverseStreamTranslator() *ReverseStreamTranslator {
	return &ReverseStreamTranslator{}
}

func (t *ReverseStreamTranslator) TranslateEvent(eventType string, data []byte) []domain.SSEChunk {
	switch eventType {
	case "message_start":
		return t.handleMessageStart(data)
	case "content_block_start":
		return t.handleContentBlockStart(data)
	case "content_block_delta":
		return t.handleContentBlockDelta(data)
	case "message_delta":
		return t.handleMessageDelta(data)
	case "message_stop":
		return []domain.SSEChunk{{Done: true}}
	}
	return nil
}

func (t *ReverseStreamTranslator) handleMessageStart(data []byte) []domain.SSEChunk {
	var evt struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	t.messageID = evt.Message.ID
	t.model = evt.Message.Model
	t.inputTokens = evt.Message.Usage.InputTokens

	chunk := &domain.OpenAIChunk{
		ID:     t.messageID,
		Object: "chat.completion.chunk",
		Model:  t.model,
		Choices: []domain.OpenAIChunkChoice{
			{Index: 0, Delta: domain.OpenAIChunkDelta{Role: "assistant"}},
		},
		Usage: &domain.OpenAIChunkUsage{
			PromptTokens: t.inputTokens,
			TotalTokens:  t.inputTokens,
		},
	}
	return []domain.SSEChunk{{Chunk: chunk}}
}

func (t *ReverseStreamTranslator) handleContentBlockStart(data []byte) []domain.SSEChunk {
	var evt struct {
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	if evt.ContentBlock.Type == "tool_use" {
		t.activeToolID = evt.ContentBlock.ID
		chunk := &domain.OpenAIChunk{
			ID:     t.messageID,
			Object: "chat.completion.chunk",
			Model:  t.model,
			Choices: []domain.OpenAIChunkChoice{
				{
					Index: 0,
					Delta: domain.OpenAIChunkDelta{
						ToolCalls: []domain.OpenAIChunkToolCall{
							{
								Index: evt.Index,
								ID:    evt.ContentBlock.ID,
								Type:  "function",
								Function: domain.OpenAIChunkFunctionCall{
									Name: evt.ContentBlock.Name,
								},
							},
						},
					},
				},
			},
		}
		return []domain.SSEChunk{{Chunk: chunk}}
	}

	// Thinking blocks are intentionally dropped in reverse translation -
	// OpenAI format has no formal thinking event type.
	return nil
}

func (t *ReverseStreamTranslator) handleContentBlockDelta(data []byte) []domain.SSEChunk {
	var evt struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	switch evt.Delta.Type {
	case "thinking_delta":
		chunk := &domain.OpenAIChunk{
			ID:      t.messageID,
			Object:  "chat.completion.chunk",
			Model:   t.model,
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{ReasoningContent: evt.Delta.Text}},
			},
		}
		return []domain.SSEChunk{{Chunk: chunk}}

	case "text_delta":
		chunk := &domain.OpenAIChunk{
			ID:      t.messageID,
			Object:  "chat.completion.chunk",
			Model:   t.model,
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: evt.Delta.Text}},
			},
		}
		return []domain.SSEChunk{{Chunk: chunk}}

	case "input_json_delta":
		if t.activeToolID != "" {
			chunk := &domain.OpenAIChunk{
				ID:      t.messageID,
				Object:  "chat.completion.chunk",
				Model:   t.model,
				Choices: []domain.OpenAIChunkChoice{
					{
						Index: 0,
						Delta: domain.OpenAIChunkDelta{
							ToolCalls: []domain.OpenAIChunkToolCall{
								{
									Index:    evt.Index,
									Function: domain.OpenAIChunkFunctionCall{Arguments: evt.Delta.PartialJSON},
								},
							},
						},
					},
				},
			}
			return []domain.SSEChunk{{Chunk: chunk}}
		}
	}

	return nil
}

func (t *ReverseStreamTranslator) handleMessageDelta(data []byte) []domain.SSEChunk {
	var evt struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	t.outputTokens = evt.Usage.OutputTokens
	finishReason := stopReasonToFinishReason(evt.Delta.StopReason)

	chunk := &domain.OpenAIChunk{
		ID:      t.messageID,
		Object:  "chat.completion.chunk",
		Model:   t.model,
		Choices: []domain.OpenAIChunkChoice{
			{Index: 0, FinishReason: &finishReason},
		},
		Usage: &domain.OpenAIChunkUsage{
			PromptTokens:     t.inputTokens,
			CompletionTokens: t.outputTokens,
			TotalTokens:      t.inputTokens + t.outputTokens,
		},
	}
	return []domain.SSEChunk{{Chunk: chunk}}
}
