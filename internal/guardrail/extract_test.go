package guardrail

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexAcceptanceStructuredMaskPreservesFields(t *testing.T) {
	tests := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"messages":[{"role":"system","content":"SECRET"},{"role":"user","content":[{"type":"text","text":"SECRET"},{"type":"image_url","image_url":{"url":"https://example.invalid/img.png"}}]},{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"send","arguments":"{\"address\":\"SECRET\"}"}}]}],"tools":[{"type":"function","function":{"name":"send","description":"SECRET"}}],"custom":{"keep":42}}`},
		{"anthropic", "/v1/messages", `{"system":[{"type":"text","text":"SECRET"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"lookup","input":{"query":"SECRET"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":[{"type":"text","text":"SECRET"},{"type":"text","text":"SECRET"}]}]}],"custom":{"keep":42}}`},
		{"responses", "/v1/responses", `{"instructions":"SECRET","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET"}]},{"type":"function_call_output","call_id":"call1","output":"SECRET"},{"type":"function_call","name":"lookup","arguments":"{\"query\":\"SECRET\"}"}],"custom":{"keep":42}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := ExtractContent([]byte(tc.body), tc.path)
			if ex == nil {
				t.Fatal("expected structured checkable content")
			}
			var spans []Span
			for offset := 0; offset < len(ex.Text); {
				i := strings.Index(ex.Text[offset:], "SECRET")
				if i < 0 {
					break
				}
				i += offset
				spans = append(spans, Span{Start: i, End: i + 6, Replacement: "[MASKED]"})
				offset = i + 6
			}
			body, ok := ex.ApplyMask(spans)
			if !ok || !json.Valid(body) {
				t.Fatalf("mask failed or invalid JSON: %s", body)
			}
			if strings.Contains(string(body), "SECRET") {
				t.Fatalf("a checkable field was not extracted or not serialized back: %s", body)
			}
			var before, after map[string]any
			_ = json.Unmarshal([]byte(tc.body), &before)
			_ = json.Unmarshal(body, &after)
			b, _ := json.Marshal(before["custom"])
			a, _ := json.Marshal(after["custom"])
			if string(a) != string(b) {
				t.Fatal("unknown fields changed")
			}
			if tc.name == "chat" && !strings.Contains(string(body), "https://example.invalid/img.png") {
				t.Fatal("media block dropped")
			}
		})
	}
}

func TestCodexAcceptanceToolJSONEscapesAreDecodedForDetection(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"SE\\u0043RET\"}"}}]}]}`},
		{"anthropic", "/v1/messages", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"lookup","input":{"query":"SE\u0043RET"}}]}]}`},
		{"responses", "/v1/responses", `{"input":[{"type":"function_call","name":"lookup","arguments":"{\"query\":\"SE\\u0043RET\"}"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := ExtractContent([]byte(tc.body), tc.path)
			if ex == nil || !strings.Contains(ex.Text, "SECRET") {
				t.Fatalf("decoded tool argument text missing from detection input: %#v", ex)
			}
		})
	}
}
