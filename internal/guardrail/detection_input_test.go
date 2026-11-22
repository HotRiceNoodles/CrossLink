package guardrail

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDetectionSegmentsProtocolsAndMask(t *testing.T) {
	tests := []struct {
		path, body                   string
		sources, roles, calls, paths []string
		indices                      []int
	}{
		{"/v1/chat/completions", `{"messages":[{"role":"system","content":"策略","approved":true},{"role":"assistant","content":"说明","reasoning_content":"思考","tool_calls":[{"id":"c1","function":{"arguments":"{\"q\":\"秘密\"}"}}]},{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"返回秘密"}]}],"tools":[{"function":{"description":"工具说明"}}]}`,
			[]string{"policy_text", "assistant", "reasoning", "tool_call", "tool_result", "tool_definition"},
			[]string{"system", "assistant", "assistant", "assistant", "tool", ""}, []string{"", "", "", "c1", "c1", ""},
			[]string{"/messages/0/content", "/messages/1/content", "/messages/1/reasoning_content", "/messages/1/tool_calls/0/function/arguments", "/messages/2/content/0/text", "/tools/0/function/description"}, []int{0, 1, 1, 1, 2, -1}},
		{"/v1/messages", `{"system":"策略","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c2","input":{"q":"秘密"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":[{"type":"text","text":"秘密"}]},{"type":"text","text":"继续"}]}]}`,
			[]string{"policy_text", "tool_call", "tool_result", "user_input"}, []string{"system", "assistant", "user", "user"}, []string{"", "c2", "c2", ""},
			[]string{"/system", "/messages/0/content/0/input", "/messages/1/content/0/content/0/text", "/messages/1/content/1/text"}, []int{-1, 0, 1, 1}},
		{"/v1/responses", `{"instructions":"策略","input":[{"role":"user","content":"问题"},{"type":"function_call","call_id":"c3","arguments":"{\"q\":\"秘密\"}"},{"type":"function_call_output","call_id":"c3","output":"秘密"}]}`,
			[]string{"policy_text", "user_input", "tool_call", "tool_result"}, []string{"system", "user", "assistant", "tool"}, []string{"", "", "c3", "c3"},
			[]string{"/instructions", "/input/0/content", "/input/1/arguments", "/input/2/output"}, []int{-1, 0, 1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			ex := ExtractContent([]byte(tt.body), tt.path)
			if ex == nil {
				t.Fatal("missing extraction")
			}
			in := ex.DetectionInput()
			if err := in.Validate(); err != nil {
				t.Fatal(err)
			}
			if in.ContextAvailable || len(in.Segments) != len(tt.sources) {
				t.Fatalf("unexpected input: %+v", in)
			}
			var spans []Span
			for i, seg := range in.Segments {
				if seg.SourceType != tt.sources[i] || seg.Role != tt.roles[i] || seg.ToolCallID != tt.calls[i] || seg.Path != tt.paths[i] || seg.MessageIndex != tt.indices[i] {
					t.Fatalf("segment %d: %+v", i, seg)
				}
				if pos := strings.Index(seg.Text, "秘密"); pos >= 0 {
					spans = append(spans, Span{Start: seg.Start + pos, End: seg.Start + pos + len("秘密"), Replacement: "已隐藏"})
				}
			}
			body, ok := ex.ApplyMask(spans)
			if !ok || !json.Valid(body) || strings.Contains(string(body), "秘密") {
				t.Fatalf("mask failed: %s", body)
			}
			again := ExtractContent(body, tt.path).DetectionInput()
			for i, seg := range again.Segments {
				if seg.ToolCallID != tt.calls[i] || seg.Role != tt.roles[i] {
					t.Fatal("mask changed protocol identity")
				}
			}
		})
	}
}

type inputCaptureEngine struct {
	input      *DetectionInput
	plainCalls int
}

func (e *inputCaptureEngine) Name() string { return "capture" }
func (e *inputCaptureEngine) Check(context.Context, string, Direction, string) (*GuardrailResult, error) {
	e.plainCalls++
	return nil, nil
}
func (e *inputCaptureEngine) CheckInput(_ context.Context, input *DetectionInput, _ Direction, _ string) (*GuardrailResult, error) {
	e.input = input
	return nil, nil
}

func TestStructuredEnginePrefixAndIsolation(t *testing.T) {
	ex := ExtractContent([]byte(`{"messages":[{"role":"user","content":"你好"},{"role":"tool","tool_call_id":"c1","content":"攻击"}]}`), "/v1/chat/completions")
	original := ex.DetectionInput()
	for _, prefix := range []string{"你好", "你好\n", "你好\n攻"} {
		e := &inputCaptureEngine{}
		_, err := checkEngine(context.Background(), e, &CheckRequest{Content: prefix, Input: original})
		if err != nil {
			t.Fatal(err)
		}
		if e.plainCalls != 0 || e.input.Coverage != "partial" || e.input.Content != prefix {
			t.Fatalf("incorrect input: %+v", e.input)
		}
		if err := e.input.Validate(); err != nil {
			t.Fatal(err)
		}
		e.input.Segments[0].Text = "mutation"
		if original.Segments[0].Text != "你好" {
			t.Fatal("engine mutated caller input")
		}
	}
	e := &inputCaptureEngine{}
	if _, err := checkEngine(context.Background(), e, &CheckRequest{Content: "different", Input: original}); err == nil {
		t.Fatal("accepted mismatched text")
	}
	if _, err := checkEngine(context.Background(), e, &CheckRequest{Content: "正文"}); err != nil || !reflect.DeepEqual(e.input, PlainDetectionInput("正文")) {
		t.Fatal("plain fallback failed")
	}
}

func TestDetectionInputRejectsInvalidSegments(t *testing.T) {
	for _, mutate := range []func(*DetectionInput){
		func(in *DetectionInput) { in.ContextAvailable = true },
		func(in *DetectionInput) { in.Segments[0].Text = "wrong" },
		func(in *DetectionInput) { in.Segments[0].End = 99 },
		func(in *DetectionInput) { in.Segments[0].Start = -1 },
		func(in *DetectionInput) { in.Segments = nil },
		func(in *DetectionInput) { in.Version = "v0" },
	} {
		in := PlainDetectionInput("测试")
		mutate(in)
		if in.Validate() == nil {
			t.Fatalf("accepted invalid input: %+v", in)
		}
	}
	media := ExtractContent([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"图片"},{"type":"image_url","image_url":{"url":"x"}}]}]}`), "/v1/chat/completions").DetectionInput()
	if media.Coverage != "partial" || media.Validate() != nil {
		t.Fatal("media coverage lost")
	}
}
