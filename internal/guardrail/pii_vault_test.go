package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func piiFixture(t *testing.T) (*PIIVault, string, string) {
	t.Helper()
	_, v, err := WithPIIVault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := "13800138000 13900139000 13800138000"
	spans := []Span{{Start: 0, End: 11, Replacement: "[PHONE]", Reversible: true}, {Start: 12, End: 23, Replacement: "[PHONE]", Reversible: true}, {Start: 24, End: 35, Replacement: "[PHONE]", Reversible: true}}
	v.maskSpans(text, spans)
	if spans[0].Replacement != spans[2].Replacement || spans[0].Replacement == spans[1].Replacement {
		t.Fatal("PII tokens must be distinct by value and stable for duplicates")
	}
	if got := v.Restore(applySpans(text, spans)); got != text {
		t.Fatalf("round trip: %q", got)
	}
	return v, spans[0].Replacement, spans[1].Replacement
}

func TestPIIVaultIsolationAndComposition(t *testing.T) {
	v, a, b := piiFixture(t)
	other, _, _ := piiFixture(t)
	if other.Restore(a) != a {
		t.Fatal("cross-request restoration")
	}
	if v.Restore("[PHONE] <PHONE_1>") != "[PHONE] <PHONE_1>" {
		t.Fatal("literal placeholder restored")
	}
	for split := 1; split < len(a); split++ {
		out, tail := v.RestorePrefix("号码："+a[:split], false)
		end, pending := v.RestorePrefix(tail+a[split:]+b, true)
		if out+end != "号码：1380013800013900139000" || pending != "" {
			t.Fatalf("split %d: %q %q", split, out+end, pending)
		}
	}
	for _, spans := range [][]Span{
		{{Start: 0, End: 11, Replacement: "[PHONE]", Reversible: true}, {Start: 0, End: 15, Replacement: "[SECRET]"}},
		{{Start: 0, End: 11, Replacement: "[PHONE]", Reversible: true}, {Start: 0, End: 11, Replacement: "[SECRET]"}},
	} {
		for _, span := range composeSpans(spans) {
			if span.Reversible {
				t.Fatal("overlapping irreversible rule became reversible")
			}
		}
	}
	encoded, _ := json.Marshal(Span{Reversible: true})
	if strings.Contains(string(encoded), "Reversible") || strings.Contains(string(encoded), "reversible") {
		t.Fatal("internal span flag exposed")
	}
}

func piiEvent(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }

func TestPIIRestoreSSEInterleavedChoicesAndTools(t *testing.T) {
	v, a, b := piiFixture(t)
	var out bytes.Buffer
	w := NewPIIRestoreSSE(&out, v, 1<<20, 100)
	write := func(s string) {
		t.Helper()
		for _, p := range []string{s[:len(s)/2], s[len(s)/2:]} {
			if _, err := w.Write([]byte(p)); err != nil {
				t.Fatal(err)
			}
		}
	}
	chat := func(idx int, delta any, final any) string {
		return piiEvent(map[string]any{"choices": []any{map[string]any{"index": idx, "delta": delta, "finish_reason": final}}})
	}
	write(chat(0, map[string]any{"content": "先发给" + a[:20]}, nil))
	write(chat(1, map[string]any{"content": b}, nil))
	write(chat(0, map[string]any{"content": a[20:]}, nil))
	write(chat(0, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `{"phone":"` + b[:15]}}}}, nil))
	write(chat(0, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": b[15:] + `"}`}}}}, nil))
	write(chat(0, map[string]any{"content": "末尾<"}, "stop"))
	write("data: [DONE]\n\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{}
	for _, frame := range strings.Split(out.String(), "\n\n") {
		if !strings.HasPrefix(frame, "data: {") {
			continue
		}
		value, err := DecodePIIJSON([]byte(strings.TrimPrefix(frame, "data: ")))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range array(object(value)["choices"]) {
			c := object(entry)
			d := object(c["delta"])
			key := fmt.Sprint(c["index"])
			texts[key] += str(d["content"])
			for _, call := range array(d["tool_calls"]) {
				texts["args"] += str(object(object(call)["function"])["arguments"])
			}
		}
	}
	if texts["0"] != "先发给13800138000末尾<" || texts["1"] != "13900139000" || texts["args"] != `{"phone":"13900139000"}` {
		t.Fatalf("incorrect stream: %#v", texts)
	}
	if strings.Contains(out.String(), "CL_PII") {
		t.Fatal("placeholder remained in client stream")
	}
}

func TestPIIRestoreSSEProtocolsAndLimits(t *testing.T) {
	v, a, _ := piiFixture(t)
	for _, protocol := range []string{"anthropic", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			var out bytes.Buffer
			w := NewPIIRestoreSSE(&out, v, 1<<20, 100)
			for i, part := range []string{a[:18], a[18:]} {
				var event any
				if protocol == "anthropic" {
					event = map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": part}}
				} else {
					event = map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": part, "sequence_number": i}
				}
				if _, err := w.Write([]byte(piiEvent(event))); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "CL_PII") || !strings.Contains(out.String(), "13800138000") {
				t.Fatal(out.String())
			}
		})
	}
	var out bytes.Buffer
	w := NewPIIRestoreSSE(&out, v, 32, 2)
	if _, err := w.Write([]byte(strings.Repeat("x", 33))); err == nil {
		t.Fatal("unbounded buffer")
	}
	w = NewPIIRestoreSSE(&out, v, 1024, 10)
	_, _ = w.Write([]byte(`data: {"delta":`))
	if err := w.Close(); err == nil {
		t.Fatal("truncated event emitted")
	}
}

func TestPIIRestoreJSONPreservesEscapesAndNumbers(t *testing.T) {
	v, a, _ := piiFixture(t)
	raw := fmt.Sprintf(`{"id":%q,"choices":[{"message":{"content":%q,"tool_calls":[{"function":{"arguments":%q}}]}}],"usage":{"total_tokens":9007199254740993}}`, a, "你好\n"+a, `{"phone":"`+a+`"}`)
	value, err := DecodePIIJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	RestorePIIJSON(value, v)
	b, _ := json.Marshal(value)
	if !json.Valid(b) || !strings.Contains(string(b), "9007199254740993") || object(value)["id"] != a {
		t.Fatal(string(b))
	}
	message := object(object(array(object(value)["choices"])[0])["message"])
	if message["content"] != "你好\n13800138000" {
		t.Fatal(message)
	}
	tool := map[string]any{"type": "tool_use", "id": a, "input": map[string]any{"recipient": a, "nested": []any{map[string]any{"phone": a}}}}
	RestorePIIJSON(tool, v)
	if tool["id"] != a || object(tool["input"])["recipient"] != "13800138000" || object(array(object(tool["input"])["nested"])[0])["phone"] != "13800138000" {
		t.Fatal(tool)
	}
	v.Clear()
	if v.Active() || v.Restore(a) != a {
		t.Fatal("request mapping survived Clear")
	}
}
