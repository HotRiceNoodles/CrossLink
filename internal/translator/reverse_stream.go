package translator

import (
	"encoding/json"

	"github.com/crosslink/internal/domain"
)

// ReverseStreamTranslator converts Anthropic SSE events into OpenAI SSE chunks.
type ReverseStreamTranslator struct {
	messageID           string
	model               string
	inputTokens         int
	outputTokens        int
	cacheReadTokens     int
	cacheCreationTokens int
	activeToolID        string
	// passthroughIdx marks block indexes whose type was unrecognized — their
	// start/delta/stop events ride verbatim (Phase-2 compact 前置).
	passthroughIdx map[int]bool
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
	case "content_block_stop":
		// Phase-2 compact 前置: the stop of a passthrough block rides
		// verbatim; stops of known blocks stay dropped (stream.go closes its
		// own blocks on type transitions, as before).
		var probe struct {
			Index int `json:"index"`
		}
		if json.Unmarshal(data, &probe) == nil && t.passthroughIdx[probe.Index] {
			return []domain.SSEChunk{{Chunk: t.passthroughChunk("content_block_stop", data)}}
		}
		return nil
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
				InputTokens              int `json:"input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	t.messageID = evt.Message.ID
	t.model = evt.Message.Model
	t.inputTokens = evt.Message.Usage.InputTokens
	t.cacheReadTokens = evt.Message.Usage.CacheReadInputTokens
	t.cacheCreationTokens = evt.Message.Usage.CacheCreationInputTokens

	// OpenAI semantics: prompt_tokens includes cached tokens (Anthropic's
	// input_tokens excludes them) — fold cache reads/writes into the total so
	// billing sees one consistent number.
	promptTotal := t.inputTokens + t.cacheReadTokens + t.cacheCreationTokens
	chunk := &domain.OpenAIChunk{
		ID:     t.messageID,
		Object: "chat.completion.chunk",
		Model:  t.model,
		Choices: []domain.OpenAIChunkChoice{
			{Index: 0, Delta: domain.OpenAIChunkDelta{Role: "assistant"}},
		},
		Usage: &domain.OpenAIChunkUsage{
			PromptTokens: promptTotal,
			TotalTokens:  promptTotal,
		},
	}
	return []domain.SSEChunk{{Chunk: chunk}}
}

func (t *ReverseStreamTranslator) handleContentBlockStart(data []byte) []domain.SSEChunk {
	var evt struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	// 类二 M-C: redacted_thinking blocks carry an opaque data payload that
	// must reach the client verbatim (no deltas ever follow for them).
	if evt.ContentBlock.Type == "redacted_thinking" {
		var raw struct {
			ContentBlock json.RawMessage `json:"content_block"`
		}
		_ = json.Unmarshal(data, &raw)
		if len(raw.ContentBlock) > 0 {
			chunk := &domain.OpenAIChunk{
				ID:     t.messageID,
				Object: "chat.completion.chunk",
				Model:  t.model,
				Choices: []domain.OpenAIChunkChoice{
					{Index: 0, Delta: domain.OpenAIChunkDelta{RedactedThinking: raw.ContentBlock}},
				},
			}
			return []domain.SSEChunk{{Chunk: chunk}}
		}
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
	// OpenAI format has no formal thinking event type. Text starts likewise
	// (stream.go opens blocks from deltas).
	if evt.ContentBlock.Type == "text" || evt.ContentBlock.Type == "thinking" {
		return nil
	}
	// Phase-2 compact 前置: any OTHER block type rides VERBATIM as a
	// passthrough event (compaction blocks etc.) — the index is recorded so
	// the block's deltas and stop passthrough too.
	if t.passthroughIdx == nil {
		t.passthroughIdx = map[int]bool{}
	}
	t.passthroughIdx[evt.Index] = true
	return []domain.SSEChunk{{Chunk: t.passthroughChunk("content_block_start", data)}}
}

// passthroughChunk wraps original anthropic event bytes in an openai chunk
// envelope so the client-facing translator can re-emit them verbatim.
func (t *ReverseStreamTranslator) passthroughChunk(event string, data []byte) *domain.OpenAIChunk {
	raw := make(json.RawMessage, len(data))
	copy(raw, data)
	return &domain.OpenAIChunk{
		ID:     t.messageID,
		Object: "chat.completion.chunk",
		Model:  t.model,
		Choices: []domain.OpenAIChunkChoice{
			{Index: 0, Delta: domain.OpenAIChunkDelta{PassthroughEvent: event, PassthroughData: raw}},
		},
	}
}

func (t *ReverseStreamTranslator) handleContentBlockDelta(data []byte) []domain.SSEChunk {
	var evt struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			Thinking    string `json:"thinking,omitempty"`
			Signature   string `json:"signature,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil
	}

	// Phase-2 compact 前置: deltas of a passthrough (unrecognized-typed)
	// block ride verbatim — their delta types are unknown to the switch below.
	if t.passthroughIdx[evt.Index] {
		return []domain.SSEChunk{{Chunk: t.passthroughChunk("content_block_delta", data)}}
	}

	switch evt.Delta.Type {
	case "thinking_delta":
		// Anthropic keys the reasoning text as "thinking" — the pre-类二 code
		// read "text" and streaming reasoning was therefore always empty for
		// real upstreams.
		chunk := &domain.OpenAIChunk{
			ID:     t.messageID,
			Object: "chat.completion.chunk",
			Model:  t.model,
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{ReasoningContent: evt.Delta.Thinking}},
			},
		}
		return []domain.SSEChunk{{Chunk: chunk}}

	case "signature_delta":
		// 类二 M-C: the thinking signature rides through the double
		// translation (OpenAI wire has no field for it) and the synthesizer
		// re-emits the signature_delta event verbatim for anthropic clients.
		chunk := &domain.OpenAIChunk{
			ID:     t.messageID,
			Object: "chat.completion.chunk",
			Model:  t.model,
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{SignatureDelta: evt.Delta.Signature}},
			},
		}
		return []domain.SSEChunk{{Chunk: chunk}}

	case "text_delta":
		chunk := &domain.OpenAIChunk{
			ID:     t.messageID,
			Object: "chat.completion.chunk",
			Model:  t.model,
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: evt.Delta.Text}},
			},
		}
		return []domain.SSEChunk{{Chunk: chunk}}

	case "input_json_delta":
		if t.activeToolID != "" {
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

	// OpenAI semantics: prompt_tokens includes cached tokens (see handleMessageStart).
	promptTotal := t.inputTokens + t.cacheReadTokens + t.cacheCreationTokens
	chunk := &domain.OpenAIChunk{
		ID:     t.messageID,
		Object: "chat.completion.chunk",
		Model:  t.model,
		Choices: []domain.OpenAIChunkChoice{
			{Index: 0, FinishReason: &finishReason},
		},
		Usage: &domain.OpenAIChunkUsage{
			PromptTokens:     promptTotal,
			CompletionTokens: t.outputTokens,
			TotalTokens:      promptTotal + t.outputTokens,
			PromptTokensDetails: func() *domain.PromptTokensDetails {
				if t.cacheReadTokens > 0 {
					return &domain.PromptTokensDetails{CachedTokens: t.cacheReadTokens}
				}
				return nil
			}(),
			CacheCreationTokens: t.cacheCreationTokens,
		},
	}
	return []domain.SSEChunk{{Chunk: chunk}}
}
