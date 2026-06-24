package translator

import (
	"github.com/crosslink/internal/domain"
)

// ResponsesStreamBuilder converts OpenAI streaming chunks (SSEChunk) into the
// Responses API streaming event sequence (3B path). It is stateful: it emits
// the lifecycle events (created / output_item.added / content_part.added) once,
// then deltas, then done/completed on Finish.
//
// This is a functional subset of the Responses streaming protocol sufficient
// for text + function_call outputs. Clients requiring the full event surface
// should use a 3A (native) provider.
type ResponsesStreamBuilder struct {
	respID    string
	model     string
	started   bool
	itemID    string            // message item id
	textAdded bool              // content_part.added emitted for the message
	textOut   string            // accumulated output_text
	toolItems map[int]*toolItem // by chunk tool_calls index
	// output_index assignment: message item first (0), then function calls 1..n
	messageEmitted bool
	nextOutIndex   int
}

type toolItem struct {
	id        string
	outIndex  int
	arguments string
	added     bool
	name      string
	callID    string
}

func NewResponsesStreamBuilder(respID, model string) *ResponsesStreamBuilder {
	if respID == "" {
		respID = generateResponseID()
	}
	return &ResponsesStreamBuilder{
		respID:   respID,
		model:    model,
		toolItems: map[int]*toolItem{},
	}
}

// Start emits the response.created event.
func (b *ResponsesStreamBuilder) Start() []domain.ResponsesEvent {
	if b.started {
		return nil
	}
	b.started = true
	return []domain.ResponsesEvent{{
		Type: "response.created",
		Payload: map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": b.respID, "object": "response", "status": "in_progress",
				"model": b.model, "output": []any{},
			},
		},
	}}
}

// Next translates an OpenAI chunk into Responses events (text + tool deltas).
func (b *ResponsesStreamBuilder) Next(chunk *domain.OpenAIChunk) []domain.ResponsesEvent {
	if chunk == nil || len(chunk.Choices) == 0 {
		return nil
	}
	var events []domain.ResponsesEvent
	delta := chunk.Choices[0].Delta

	if delta.Content != "" {
		if !b.messageEmitted {
			b.itemID = generateMessageID()
			b.messageEmitted = true
			b.nextOutIndex = 1 // message occupies index 0
			events = append(events,
				b.outputItemAdded(b.itemID, 0, "message"),
				b.contentPartAdded(b.itemID, 0),
			)
			b.textAdded = true
		}
		b.textOut += delta.Content
		events = append(events, domain.ResponsesEvent{
			Type: "response.output_text.delta",
			Payload: map[string]any{
				"type": "response.output_text.delta", "item_id": b.itemID,
				"output_index": 0, "content_index": 0, "delta": delta.Content,
			},
		})
	}

	for _, tc := range delta.ToolCalls {
		ti := b.toolItems[tc.Index]
		if ti == nil {
			ti = &toolItem{
				id:       generateMessageID(),
				outIndex: b.nextOutIndex,
				name:     tc.Function.Name,
				callID:   tc.ID,
			}
			b.nextOutIndex++
			b.toolItems[tc.Index] = ti
		}
		if tc.Function.Name != "" {
			ti.name = tc.Function.Name
		}
		if tc.ID != "" {
			ti.callID = tc.ID
		}
		if !ti.added {
			ti.added = true
			events = append(events, b.functionCallItemAdded(ti))
		}
		if tc.Function.Arguments != "" {
			ti.arguments += tc.Function.Arguments
			events = append(events, domain.ResponsesEvent{
				Type: "response.function_call_arguments.delta",
				Payload: map[string]any{
					"type": "response.function_call_arguments.delta",
					"item_id": ti.id, "output_index": ti.outIndex,
					"delta": tc.Function.Arguments,
				},
			})
		}
	}
	return events
}

// Finish emits done + completed events using the final usage and finish reason.
func (b *ResponsesStreamBuilder) Finish(usage domain.OpenAIUsage, finishReason, model string) []domain.ResponsesEvent {
	var events []domain.ResponsesEvent
	if model == "" {
		model = b.model
	}

	if b.textAdded {
		events = append(events,
			domain.ResponsesEvent{Type: "response.output_text.done", Payload: map[string]any{
				"type": "response.output_text.done", "item_id": b.itemID,
				"output_index": 0, "content_index": 0, "text": b.textOut,
			}},
			domain.ResponsesEvent{Type: "response.content_part.done", Payload: map[string]any{
				"type": "response.content_part.done", "item_id": b.itemID,
				"output_index": 0, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": b.textOut},
			}},
		)
	}
	if b.messageEmitted {
		events = append(events, b.messageItemDone())
	}

	// function_call done events in output_index order
	ordered := b.orderedToolItems()
	for _, ti := range ordered {
		events = append(events,
			domain.ResponsesEvent{Type: "response.function_call_arguments.done", Payload: map[string]any{
				"type": "response.function_call_arguments.done", "item_id": ti.id,
				"output_index": ti.outIndex, "arguments": ti.arguments,
			}},
			domain.ResponsesEvent{Type: "response.output_item.done", Payload: map[string]any{
				"type": "response.output_item.done", "output_index": ti.outIndex,
				"item": map[string]any{
					"type": "function_call", "id": ti.id, "status": "completed",
					"call_id": ti.callID, "name": ti.name, "arguments": ti.arguments,
				},
			}},
		)
	}

	status := "completed"
	if finishReason == "length" || finishReason == "content_filter" {
		status = "incomplete"
	}
	// Assemble final output array for response.completed
	finalOutput := b.finalOutput()
	events = append(events, domain.ResponsesEvent{Type: "response.completed", Payload: map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": b.respID, "object": "response", "status": status, "model": model,
			"output": finalOutput, "usage": ResponsesUsageFromOpenAI(usage),
		},
	}})
	return events
}

func (b *ResponsesStreamBuilder) orderedToolItems() []*toolItem {
	out := make([]*toolItem, 0, len(b.toolItems))
	seen := map[int]bool{}
	// stable by outIndex
	maxIdx := b.nextOutIndex
	for oi := 0; oi < maxIdx; oi++ {
		for idx, ti := range b.toolItems {
			if ti.outIndex == oi && !seen[idx] {
				out = append(out, ti)
				seen[idx] = true
			}
		}
	}
	return out
}

func (b *ResponsesStreamBuilder) finalOutput() []map[string]any {
	var out []map[string]any
	if b.messageEmitted {
		out = append(out, map[string]any{
			"type": "message", "id": b.itemID, "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": b.textOut}},
		})
	}
	for _, ti := range b.orderedToolItems() {
		out = append(out, map[string]any{
			"type": "function_call", "id": ti.id, "status": "completed",
			"call_id": ti.callID, "name": ti.name, "arguments": ti.arguments,
		})
	}
	return out
}

func (b *ResponsesStreamBuilder) outputItemAdded(itemID string, outIndex int, itemType string) domain.ResponsesEvent {
	return domain.ResponsesEvent{Type: "response.output_item.added", Payload: map[string]any{
		"type": "response.output_item.added", "output_index": outIndex,
		"item": map[string]any{
			"type": itemType, "id": itemID, "role": "assistant",
			"status": "in_progress", "content": []any{},
		},
	}}
}

func (b *ResponsesStreamBuilder) contentPartAdded(itemID string, outIndex int) domain.ResponsesEvent {
	return domain.ResponsesEvent{Type: "response.content_part.added", Payload: map[string]any{
		"type": "response.content_part.added", "item_id": itemID,
		"output_index": outIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": ""},
	}}
}

func (b *ResponsesStreamBuilder) functionCallItemAdded(ti *toolItem) domain.ResponsesEvent {
	return domain.ResponsesEvent{Type: "response.output_item.added", Payload: map[string]any{
		"type": "response.output_item.added", "output_index": ti.outIndex,
		"item": map[string]any{
			"type": "function_call", "id": ti.id, "status": "in_progress",
			"call_id": ti.callID, "name": ti.name, "arguments": "",
		},
	}}
}

func (b *ResponsesStreamBuilder) messageItemDone() domain.ResponsesEvent {
	return domain.ResponsesEvent{Type: "response.output_item.done", Payload: map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{
			"type": "message", "id": b.itemID, "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": b.textOut}},
		},
	}}
}
