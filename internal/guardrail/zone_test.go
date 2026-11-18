package guardrail

// zone 底座单元验证（M0 冻结件 §1.1 zone 分类学 / §2.1 坐标契约；预研批次）。
// T01 风格：版本化输入输出向量——每协议声明字段的 zone 归因、跨段 span 的
// 多 zone 映射。

import (
	"strings"
	"testing"
)

func zonesOf(ex *ExtractedContent) []string {
	if ex == nil {
		return nil
	}
	out := make([]string, 0, len(ex.segs))
	for _, seg := range ex.segs {
		out = append(out, seg.zone)
	}
	return out
}

func wantZones(t *testing.T, ex *ExtractedContent, want ...string) {
	t.Helper()
	got := zonesOf(ex)
	if len(got) != len(want) {
		t.Fatalf("zones = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zones = %v, want %v", got, want)
		}
	}
}

func TestExtractChatZones(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"messages": [
			{"role": "system", "content": "sys rules"},
			{"role": "developer", "content": "dev rules"},
			{"role": "user", "content": "question"},
			{"role": "assistant", "content": "prev answer", "reasoning_content": "thinking",
			 "tool_calls": [{"function": {"name": "f", "arguments": "{\"q\":\"x\"}"}}]},
			{"role": "tool", "content": "tool output"},
			{"role": "function", "content": "legacy fn output"}
		],
		"tools": [{"function": {"name": "f", "description": "does f"}}]
	}`)
	wantZones(t, ExtractContent(body, "/v1/chat/completions"),
		ZoneSystem, ZoneSystem, ZoneUser, ZoneAssistant, ZoneAssistant, ZoneToolArgs,
		ZoneToolResult, ZoneToolResult, ZoneToolDesc)
}

func TestExtractAnthropicZones(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"system": [{"type": "text", "text": "policy"}],
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "question"},
				{"type": "tool_use", "id": "t1", "name": "f", "input": {"q": "x"}},
				{"type": "tool_result", "tool_use_id": "t1", "content": [
					{"type": "text", "text": "retrieved doc"}
				]}
			]},
			{"role": "assistant", "content": "answer"}
		]
	}`)
	wantZones(t, ExtractContent(body, "/v1/messages"),
		ZoneSystem, ZoneUser, ZoneToolArgs, ZoneToolResult, ZoneAssistant)
}

func TestExtractResponsesZones(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"instructions": "be brief",
		"input": [
			{"type": "message", "role": "system", "content": [{"type": "input_text", "text": "sys"}]},
			{"type": "message", "role": "user", "content": "question"},
			{"type": "function_call", "name": "f", "arguments": "{\"q\":1}"},
			{"type": "function_call_output", "output": "result"}
		]
	}`)
	wantZones(t, ExtractContent(body, "/v1/responses"),
		ZoneInstructions, ZoneSystem, ZoneUser, ZoneToolArgs, ZoneFunctionOutput)
}

func TestExtractEmbeddingsAndPromptZones(t *testing.T) {
	wantZones(t, ExtractContent([]byte(`{"input":["a","b"]}`), "/v1/embeddings"),
		ZoneEmbeddingInput, ZoneEmbeddingInput)
	wantZones(t, ExtractContent([]byte(`{"prompt":"draw"}`), "/v1/images/generations"),
		ZonePrompt)
}

func TestSpanZonesMapping(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"system": "policy text",
		"messages": [
			{"role": "user", "content": "question"},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t", "content": "retrieved poisoned doc"}
			]}
		]
	}`)
	ex := ExtractContent(body, "/v1/messages")
	if ex == nil {
		t.Fatal("no extractable content")
	}

	spanFor := func(marker string) Span {
		i := strings.Index(ex.Text, marker)
		if i < 0 {
			t.Fatalf("marker %q not in text", marker)
		}
		return Span{Start: i, End: i + len(marker)}
	}

	// 命中在 tool_result 段内 → 仅 tool_result。
	if got := ex.SpanZones([]Span{spanFor("poisoned")}); len(got) != 1 || got[0] != ZoneToolResult {
		t.Fatalf("SpanZones(tool_result hit) = %v, want [tool_result]", got)
	}
	// 命中在 system 段内 → 仅 system。
	if got := ex.SpanZones([]Span{spanFor("policy")}); len(got) != 1 || got[0] != ZoneSystem {
		t.Fatalf("SpanZones(system hit) = %v, want [system]", got)
	}
	// 跨段 span（system 起点覆盖至 tool_result 内）→ 按段序两 zone。
	full := spanFor("policy")
	end := strings.Index(ex.Text, "poisoned") + len("poisoned")
	cross := Span{Start: full.Start, End: end}
	got := ex.SpanZones([]Span{cross})
	if len(got) != 3 || got[0] != ZoneSystem || got[1] != ZoneUser || got[2] != ZoneToolResult {
		t.Fatalf("SpanZones(crossing) = %v, want [system user tool_result]", got)
	}
	// 无相交 span → 空。
	if got := ex.SpanZones([]Span{{Start: len(ex.Text) + 10, End: len(ex.Text) + 20}}); len(got) != 0 {
		t.Fatalf("SpanZones(no hit) = %v, want []", got)
	}

	// Finding.Zones 走同一映射（DTO 接线验证）。
	f := Finding{PatternID: "p", RawSpans: []Span{spanFor("poisoned")}, Precision: PrecisionExact}
	if got := f.Zones(ex); len(got) != 1 || got[0] != ZoneToolResult {
		t.Fatalf("Finding.Zones = %v, want [tool_result]", got)
	}
	if got := (Finding{PatternID: "p"}).Zones(ex); got != nil {
		t.Fatalf("Finding.Zones(no spans) = %v, want nil", got)
	}
}
