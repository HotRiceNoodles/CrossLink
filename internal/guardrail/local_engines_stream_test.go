package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
)

func TestIncrementalLocalEnginesProtocolReleaseAndBlock(t *testing.T) {
	bfSecret := credentialFixtures()["sk_bf_key"]
	for _, tc := range []struct{ name, kind, candidate, ending string }{
		{name: "content_length", kind: "content_length", ending: "超限后文"},
		{name: "credential_detection", kind: "credential_detection", candidate: "sk-" + strings.Repeat("Q", 10), ending: strings.Repeat("Q", 10) + "超限后文"},
		{name: "sk_bf_key", kind: "credential_detection", candidate: bfSecret[:len(bfSecret)-1], ending: bfSecret[len(bfSecret)-1:] + "超限后文"},
	} {
		kind := tc.kind
		for _, protocol := range []string{"chat", "anthropic", "responses"} {
			t.Run(tc.name+"/"+protocol, func(t *testing.T) {
				svc := incrementalService()
				svc.rules[0].Type = kind
				svc.rules[0].Config = datatypes.JSON(`{"max_output_chars":8}`)
				if kind == "credential_detection" {
					svc.rules[0].Config = datatypes.JSON(`{"categories":["api_key"]}`)
				}
				events := make(chan GuardrailEvent, 2)
				svc.AddHandler(func(_ context.Context, event GuardrailEvent) { events <- event })
				w, out := incrementalWriter(t, svc)
				delta := func(text string) string {
					switch protocol {
					case "anthropic":
						return frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
					case "responses":
						return frame("response.output_text.delta", map[string]any{"item_id": "msg1", "delta": text})
					default:
						return chatFrame(0, text)
					}
				}
				first := delta("安全文字。")
				writeOK(t, w, first)
				if out.String() != first {
					t.Fatal("正常文本没有立即放行")
				}
				if kind == "content_length" {
					writeOK(t, w, delta("继续。"))
				} else {
					writeOK(t, w, delta("候选"+tc.candidate))
					if !strings.Contains(out.String(), "候选") || strings.Contains(out.String(), "sk-") {
						t.Fatal("同帧前文未放行或凭据候选泄露")
					}
				}
				if _, err := w.Write([]byte(delta(tc.ending))); !errors.Is(err, ErrStreamBlocked) {
					t.Fatal("没有立即截停", err)
				}
				if strings.Contains(out.String(), "超限后文") || !strings.Contains(out.String(), "guardrail_blocked") {
					t.Fatal(out.String())
				}
				select {
				case event := <-events:
					if event.EngineType != kind || event.ContentPreview == "" {
						t.Fatalf("事件预览或归因遗漏：%+v", event)
					}
					if tc.name == "sk_bf_key" && event.ContentPreview != "[CREDENTIAL:sk_bf_key]" {
						t.Fatalf("新增格式没有安全的预览归因：%+v", event)
					}
				case <-time.After(time.Second):
					t.Fatal("没有阻断事件")
				}
			})
		}
	}
}

func TestIncrementalCredentialToolArgumentsDecodeBeforeRelease(t *testing.T) {
	svc := incrementalService()
	svc.rules[0].Type = "credential_detection"
	svc.rules[0].Config = datatypes.JSON(`{"categories":["api_key"]}`)
	w, out := incrementalWriter(t, svc)
	args := `{"key":"\u0073\u006b\u002d` + strings.Repeat("Q", 20) + `"}`
	for _, part := range []string{args[:24], args[24:]} {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "send", "arguments": part}}}}}}})
		writeOK(t, w, "data: "+string(raw)+"\n\n")
	}
	if out.Len() != 0 {
		t.Fatal("未批准的工具调用提前放行")
	}
	_, err := w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
	if !errors.Is(err, ErrStreamBlocked) || strings.Contains(out.String(), "tool_calls") {
		t.Fatalf("工具 JSON 转义凭据漏拦：%v %s", err, out.String())
	}
}
