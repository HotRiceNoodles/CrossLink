package translator

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
)

func TestResponsesToOpenAI_StringInput(t *testing.T) {
	req := &domain.ResponsesRequest{
		Model:    "gpt-4o",
		Input:    json.RawMessage(`"hello"`),
		Instructions: "be brief",
	}
	out, err := ResponsesToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("expected system + user = 2 messages, got %d", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "be brief" {
		t.Errorf("instructions not mapped to leading system: %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "hello" {
		t.Errorf("string input not mapped to user message: %+v", out.Messages[1])
	}
}

func TestResponsesToOpenAI_FunctionCallOutputItem(t *testing.T) {
	items := []domain.ResponsesInputItem{
		{Type: "message", Role: "user", Content: json.RawMessage(`"what's the weather?"`)},
		{Type: "function_call", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"SF"}`},
		{Type: "function_call_output", CallID: "call_1", Output: `{"temp":60}`},
		{Type: "message", Role: "user", Content: json.RawMessage(`"thanks"`)},
	}
	raw, _ := json.Marshal(items)
	req := &domain.ResponsesRequest{Model: "gpt-4o", Input: raw}
	out, err := ResponsesToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out.Messages))
	}
	if out.Messages[1].Role != "assistant" || len(out.Messages[1].ToolCalls) != 1 {
		t.Errorf("function_call not mapped to assistant tool_calls: %+v", out.Messages[1])
	}
	tc := out.Messages[1].ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"SF"}` {
		t.Errorf("function_call fields wrong: %+v", tc)
	}
	if out.Messages[2].Role != "tool" || out.Messages[2].ToolCallID != "call_1" || out.Messages[2].Content != `{"temp":60}` {
		t.Errorf("function_call_output not mapped to tool message: %+v", out.Messages[2])
	}
}

func TestResponsesToOpenAI_ToolsStrictPassthrough(t *testing.T) {
	strict := true
	req := &domain.ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`"hi"`),
		Tools: []domain.ResponsesTool{{
			Type: "function",
			Function: domain.ResponsesToolFunc{
				Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict,
			},
		}},
	}
	out, err := ResponsesToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "lookup" {
		t.Fatalf("tool not mapped: %+v", out.Tools)
	}
	if out.Tools[0].Function.Strict == nil || *out.Tools[0].Function.Strict != true {
		t.Errorf("strict not passed through: %+v", out.Tools[0].Function.Strict)
	}
}

func TestResponsesToOpenAI_MaxOutputAndReasoning(t *testing.T) {
	maxTok := 500
	req := &domain.ResponsesRequest{
		Model:           "o3",
		Input:           json.RawMessage(`"hi"`),
		MaxOutputTokens: &maxTok,
		Reasoning:       json.RawMessage(`{"effort":"high"}`),
	}
	out, err := ResponsesToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 500 {
		t.Errorf("max_output_tokens not mapped to max_tokens: %+v", out.MaxTokens)
	}
	if out.ReasoningEffort != "high" {
		t.Errorf("reasoning.effort not mapped: %q", out.ReasoningEffort)
	}
}

func TestOpenAIToResponses_TextAndToolCalls(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message: domain.OpenAIMessage{
				Role:    "assistant",
				Content: "let me check",
				ToolCalls: []domain.OpenAIToolCall{{
					ID: "call_9", Type: "function",
					Function: domain.OpenAIFunctionCall{Name: "search", Arguments: `{"q":"x"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: domain.OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	out := OpenAIToResponses(resp, "gpt-4o")
	if out.Object != "response" {
		t.Errorf("object should be response: %q", out.Object)
	}
	if out.Status != "completed" {
		t.Errorf("tool_calls finish should be completed: %q", out.Status)
	}
	if len(out.Output) != 2 {
		t.Fatalf("expected message + function_call = 2 items, got %d", len(out.Output))
	}
	if out.Output[0].Type != "message" || len(out.Output[0].Content) != 1 || out.Output[0].Content[0].Text != "let me check" {
		t.Errorf("message item wrong: %+v", out.Output[0])
	}
	if out.Output[1].Type != "function_call" || out.Output[1].CallID != "call_9" || out.Output[1].Name != "search" || out.Output[1].Arguments != `{"q":"x"}` {
		t.Errorf("function_call item wrong: %+v", out.Output[1])
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 || out.Usage.TotalTokens != 15 {
		t.Errorf("usage mapping wrong: %+v", out.Usage)
	}
}

func TestOpenAIToResponses_LengthIsIncomplete(t *testing.T) {
	resp := &domain.OpenAIResponse{
		Choices: []domain.OpenAIChoice{{
			Message:      domain.OpenAIMessage{Role: "assistant", Content: "cut off"},
			FinishReason: "length",
		}},
	}
	out := OpenAIToResponses(resp, "gpt-4o")
	if out.Status != "incomplete" {
		t.Errorf("length finish should be incomplete: %q", out.Status)
	}
}

func TestSupportsResponsesFlag(t *testing.T) {
	if SupportsResponses(nil) {
		t.Error("nil should be false")
	}
	if SupportsResponses(json.RawMessage(`{}`)) {
		t.Error("empty object should be false")
	}
	if !SupportsResponses(json.RawMessage(`{"supports_responses":true}`)) {
		t.Error("true flag should be true")
	}
	if SupportsResponses(json.RawMessage(`{"supports_responses":false}`)) {
		t.Error("false flag should be false")
	}
}

func TestResponsesStreamBuilder_TextLifecycle(t *testing.T) {
	b := NewResponsesStreamBuilder("resp_1", "gpt-4o")
	events := b.Start()
	if len(events) != 1 || events[0].Type != "response.created" {
		t.Fatalf("Start should emit response.created, got %+v", events)
	}
	chunk := func(content string) *domain.OpenAIChunk {
		return &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{Delta: domain.OpenAIChunkDelta{Content: content}}}}
	}
	e1 := b.Next(chunk("hel"))
	e2 := b.Next(chunk("lo"))
	if len(e1) < 3 { // output_item.added + content_part.added + delta
		t.Errorf("first chunk should emit item+part+delta, got %d events", len(e1))
	}
	if e1[0].Type != "response.output_item.added" || e1[1].Type != "response.content_part.added" {
		t.Errorf("first two events wrong: %s, %s", e1[0].Type, e1[1].Type)
	}
	if len(e2) != 1 || e2[0].Type != "response.output_text.delta" {
		t.Errorf("second chunk should emit only delta, got %+v", e2)
	}
	fin := b.Finish(domain.OpenAIUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, "stop", "gpt-4o")
	// output_text.done + content_part.done + output_item.done(message) + response.completed
	if len(fin) < 4 {
		t.Errorf("Finish should emit dones + completed, got %d", len(fin))
	}
	if fin[len(fin)-1].Type != "response.completed" {
		t.Errorf("last finish event should be response.completed: %+v", fin[len(fin)-1])
	}
}

func TestResponsesStreamBuilder_FunctionCall(t *testing.T) {
	b := NewResponsesStreamBuilder("resp_2", "gpt-4o")
	b.Start()
	toolChunk := &domain.OpenAIChunk{Choices: []domain.OpenAIChunkChoice{{
		Delta: domain.OpenAIChunkDelta{
			ToolCalls: []domain.OpenAIChunkToolCall{{
				Index: 0, ID: "call_1",
				Function: domain.OpenAIChunkFunctionCall{Name: "f", Arguments: `{"a":1}`},
			}},
		},
	}}}
	ev := b.Next(toolChunk)
	hasAdded := false
	hasArgDelta := false
	for _, e := range ev {
		if e.Type == "response.output_item.added" {
			hasAdded = true
		}
		if e.Type == "response.function_call_arguments.delta" {
			hasArgDelta = true
		}
	}
	if !hasAdded || !hasArgDelta {
		t.Errorf("tool chunk should emit output_item.added + function_call_arguments.delta, got %+v", ev)
	}
	fin := b.Finish(domain.OpenAIUsage{}, "tool_calls", "gpt-4o")
	hasFnDone := false
	for _, e := range fin {
		if e.Type == "response.function_call_arguments.done" {
			hasFnDone = true
		}
	}
	if !hasFnDone {
		t.Errorf("Finish should emit function_call_arguments.done")
	}
}
