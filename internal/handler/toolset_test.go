package handler

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/crosslink/internal/domain"
)

type fakeToolObserver struct {
	obs []domain.ToolObservation
}

func (f *fakeToolObserver) Observe(o domain.ToolObservation) { f.obs = append(f.obs, o) }

func openAIToolReq(tools []domain.OpenAITool) *domain.OpenAIRequest {
	return &domain.OpenAIRequest{Model: "gpt-4o", Tools: tools}
}

func TestBuildOpenAIToolObservation(t *testing.T) {
	req := openAIToolReq([]domain.OpenAITool{
		{Type: "function", Function: domain.OpenAIFunctionDef{
			Name: "search", Description: "web search",
			Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		{Type: "function", Function: domain.OpenAIFunctionDef{Name: ""}}, // skipped
	})
	resp := &domain.OpenAIResponse{Choices: []domain.OpenAIChoice{{
		Message: domain.OpenAIMessage{ToolCalls: []domain.OpenAIToolCall{{
			Function: domain.OpenAIFunctionCall{Name: "search"},
		}}},
	}}}

	obs := buildOpenAIToolObservation(toolsetInput{keyID: 7, teamID: 3, orgID: 1, model: "gpt-4o"}, req, resp)

	if obs.Protocol != "openai" || obs.Source != "proxy" || obs.Model != "gpt-4o" {
		t.Fatalf("meta = %+v", obs)
	}
	if obs.APIKeyID != 7 || obs.TeamID != 3 || obs.OrgID != 1 {
		t.Fatalf("identity = %+v", obs)
	}
	if len(obs.Declared) != 1 {
		t.Fatalf("declared = %d, want 1", len(obs.Declared))
	}
	d := obs.Declared[0]
	if d.Name != "search" || d.Description != "web search" || string(d.SchemaRaw) == "" {
		t.Fatalf("declared[0] = %+v", d)
	}
	if len(obs.Invoked) != 1 || obs.Invoked[0] != "search" {
		t.Fatalf("invoked = %v", obs.Invoked)
	}
}

func TestBuildOpenAIToolObservation_CacheHitSkipsInvoked(t *testing.T) {
	req := openAIToolReq([]domain.OpenAITool{
		{Function: domain.OpenAIFunctionDef{Name: "search"}},
	})
	resp := &domain.OpenAIResponse{Choices: []domain.OpenAIChoice{{
		Message: domain.OpenAIMessage{ToolCalls: []domain.OpenAIToolCall{{
			Function: domain.OpenAIFunctionCall{Name: "search"},
		}}},
	}}}

	obs := buildOpenAIToolObservation(toolsetInput{keyID: 7, cacheHit: true, invoked: []string{"search"}}, req, resp)

	if len(obs.Declared) != 1 {
		t.Fatalf("declared = %d, want 1 on cache hit", len(obs.Declared))
	}
	if len(obs.Invoked) != 0 {
		t.Fatalf("invoked = %v, want none on cache hit", obs.Invoked)
	}
}

func TestBuildOpenAIToolObservation_DeclaredCap(t *testing.T) {
	tools := make([]domain.OpenAITool, 200)
	for i := range tools {
		tools[i].Function.Name = "t" + strconv.Itoa(i)
	}
	obs := buildOpenAIToolObservation(toolsetInput{keyID: 1}, openAIToolReq(tools), nil)
	if len(obs.Declared) != maxDeclaredTools {
		t.Fatalf("declared = %d, want cap %d", len(obs.Declared), maxDeclaredTools)
	}
}

func TestBuildAnthropicToolObservation(t *testing.T) {
	req := &domain.AnthropicRequest{
		Model: "claude-3-5",
		Tools: json.RawMessage(`[
			{"name":"get_weather","description":"weather lookup","input_schema":{"type":"object"}},
			{"name":"","input_schema":{"type":"object"}}
		]`),
	}
	resp := &domain.AnthropicResponse{Content: []domain.ContentBlock{
		{Type: "text", Text: "hi"},
		{Type: "tool_use", Name: "get_weather", ID: "t1"},
	}}

	obs := buildAnthropicToolObservation(toolsetInput{keyID: 9, orgID: 2, model: "claude-3-5"}, req, resp)

	if obs.Protocol != "anthropic" {
		t.Fatalf("protocol = %s", obs.Protocol)
	}
	if len(obs.Declared) != 1 || obs.Declared[0].Name != "get_weather" || obs.Declared[0].Description != "weather lookup" {
		t.Fatalf("declared = %+v", obs.Declared)
	}
	if len(obs.Invoked) != 1 || obs.Invoked[0] != "get_weather" {
		t.Fatalf("invoked = %v", obs.Invoked)
	}
}

func TestBuildAnthropicToolObservation_BadToolsJSON(t *testing.T) {
	req := &domain.AnthropicRequest{Tools: json.RawMessage(`{not-json}`)}
	resp := &domain.AnthropicResponse{Content: []domain.ContentBlock{{Type: "tool_use", Name: "x"}}}

	obs := buildAnthropicToolObservation(toolsetInput{keyID: 9}, req, resp)

	if len(obs.Declared) != 0 {
		t.Fatalf("declared = %d, want 0 on unparseable tools", len(obs.Declared))
	}
	if len(obs.Invoked) != 1 {
		t.Fatalf("invoked = %v, want response names kept", obs.Invoked)
	}
}

func TestObserveToolsGuards(t *testing.T) {
	full := domain.ToolObservation{APIKeyID: 5, Declared: []domain.DeclaredTool{{Name: "a"}}}
	empty := domain.ToolObservation{APIKeyID: 5}
	noKey := domain.ToolObservation{Declared: []domain.DeclaredTool{{Name: "a"}}}

	// nil observer must not panic and builds nothing.
	observeTools(nil, full)

	f := &fakeToolObserver{}
	observeTools(f, empty) // nothing to observe
	observeTools(f, noKey) // no key identity
	if len(f.obs) != 0 {
		t.Fatalf("obs = %d, want 0", len(f.obs))
	}
	observeTools(f, full)
	if len(f.obs) != 1 || f.obs[0].Declared[0].Name != "a" {
		t.Fatalf("obs = %+v", f.obs)
	}
}

func TestStreamToolCallNames(t *testing.T) {
	delta := domain.OpenAIChunkDelta{ToolCalls: []domain.OpenAIChunkToolCall{
		{Index: 0, Function: domain.OpenAIChunkFunctionCall{Name: "search"}},
		{Index: 1, Function: domain.OpenAIChunkFunctionCall{Name: ""}}, // argument fragment
	}}
	names := streamToolCallNames(delta)
	if len(names) != 1 || names[0] != "search" {
		t.Fatalf("names = %v", names)
	}
}
