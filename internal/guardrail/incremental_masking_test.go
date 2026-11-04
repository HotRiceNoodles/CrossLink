package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

type completeMaskEngine struct{}

func (completeMaskEngine) Name() string { return "complete_mask_test" }
func (completeMaskEngine) Check(_ context.Context, text string, _ Direction, _ string) (*GuardrailResult, error) {
	return &GuardrailResult{Blocked: strings.Contains(text, "SECRET"), Reason: "sensitive content"}, nil
}

func TestIncrementalMaskUsesSharedQueue(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "安全，SE"))
	if !strings.Contains(out.String(), "安全，") || strings.Contains(out.String(), "SE") {
		t.Fatalf("安全前文未立即放行: %s", out.String())
	}
	writeOK(t, w, chatFrame(0, "CRET，继续 SECRET。"))
	writeOK(t, w, "data: [DONE]\n\n")
	if strings.Contains(out.String(), "SECRET") || strings.Count(out.String(), "[REDACTED]") != 2 || !strings.Contains(out.String(), "继续") || w.Result != nil && w.Result.Blocked {
		t.Fatalf("脱敏输出错误: %s", out.String())
	}
}

func TestIncrementalConservativeMaskWaitsForWholeResponse(t *testing.T) {
	RegisterEngine("complete_mask_test", func(json.RawMessage) (GuardrailEngine, error) { return completeMaskEngine{}, nil })
	s := incrementalService()
	s.rules[0].Type = "complete_mask_test"
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "SECRET"))
	if out.Len() != 0 {
		t.Fatal("完整检查前泄露正文")
	}
	writeOK(t, w, "data: [DONE]\n\n")
	if strings.Contains(out.String(), "SECRET") || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatalf("仅命中结论未遮盖: %s", out.String())
	}
}

func TestIncrementalBlockReleasesSameFrameSafePrefix(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	if _, err := w.Write([]byte(chatFrame(0, "安全，SECRET 后文"))); err == nil {
		t.Fatal("未阻断")
	}
	if !strings.Contains(out.String(), "安全，") || strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "后文") {
		t.Fatalf("阻断边界错误: %s", out.String())
	}
}

func TestIncrementalCompleteMaskDoesNotBecomeBlock(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	r, err := s.NewResponseStream(context.Background(), CheckRequest{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	result, err := r.CheckComplete(context.Background(), "SECRET")
	if err != nil || result == nil || result.Blocked || result.Action != "mask" || result.MaskedContent != "[REDACTED]" {
		t.Fatalf("完整字段脱敏被当作拒绝: %+v %v", result, err)
	}
}

func TestIncrementalMasksToolsAndSnapshots(t *testing.T) {
	for _, protocol := range []string{"chat", "anthropic", "responses", "snapshot"} {
		t.Run(protocol, func(t *testing.T) {
			s := incrementalService()
			s.rules[0].Action = "mask"
			w, out := incrementalWriter(t, s)
			switch protocol {
			case "chat":
				writeOK(t, w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"{\"value\":\"SEC"}}]}}]}`+"\n\n")
				if out.Len() != 0 {
					t.Fatal("工具开始事件提前发送")
				}
				writeOK(t, w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"RET\",\"count\":2}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
			case "anthropic":
				writeOK(t, w, frame("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": "lookup", "input": map[string]any{}}}))
				writeOK(t, w, frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"value":"\u0053ECRET","count":2}`}}))
				writeOK(t, w, frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
			case "responses":
				writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "m1", "delta": "SECRET"}))
				writeOK(t, w, frame("response.output_text.done", map[string]any{"item_id": "m1", "text": "SECRET"}))
				writeOK(t, w, frame("response.completed", map[string]any{"response": map[string]any{"output": []any{map[string]any{"id": "m1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "SECRET"}}}}}}))
			case "snapshot":
				writeOK(t, w, frame("custom.complete", map[string]any{"content": "SECRET"}))
			}
			writeOK(t, w, "data: [DONE]\n\n")
			if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), `\u0053ECRET`) || !strings.Contains(out.String(), "[REDACTED]") {
				t.Fatalf("字段未改写: %s", out.String())
			}
		})
	}
}

func TestIncrementalConservativeChangedSnapshotIsCheckedAsWhole(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	s.rules[0].Config = []byte(`{"use_regex":true,"blocklist":["SEC.*RET"]}`)
	w, out := incrementalWriter(t, s)
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "m1", "delta": "safe"}))
	writeOK(t, w, frame("response.output_text.done", map[string]any{"item_id": "m1", "text": "SECRET"}))
	writeOK(t, w, "data: [DONE]\n\n")
	if strings.Contains(out.String(), "SECRET") || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatalf("替换快照绕过完整审核: %s", out.String())
	}
}

func TestIncrementalLogCandidateDoesNotRevokeReleasedText(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "log"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "SE"))
	if !strings.Contains(out.String(), "SE") {
		t.Fatal("log 暂留正文")
	}
	writeOK(t, w, chatFrame(0, "CRET"))
	writeOK(t, w, "data: [DONE]\n\n")
	if w.Result != nil && w.Result.Blocked {
		t.Fatal("log被误阻断")
	}
}

func TestIncrementalAuditErrorHasSeparateReason(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	if _, err := w.Write([]byte("data: invalid\n\n")); err == nil {
		t.Fatal("解析错误未终止")
	}
	if !strings.Contains(out.String(), `"reason_code":"parse_error"`) || strings.Contains(out.String(), `"reason_code":"policy_match"`) {
		t.Fatalf("审核错误被混为命中: %s", out.String())
	}
}

func TestIncrementalToolMaskPreservesLargeJSONNumbers(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	args := `{"text":"SECRET","number":9007199254740993}`
	event := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "lookup", "arguments": args}}}}, "finish_reason": "tool_calls"}}}
	raw, _ := json.Marshal(event)
	writeOK(t, w, "data: "+string(raw)+"\n\n")
	writeOK(t, w, "data: [DONE]\n\n")
	if !strings.Contains(out.String(), "9007199254740993") || strings.Contains(out.String(), "SECRET") {
		t.Fatalf("工具数值或脱敏结构改变: %s", out.String())
	}
}

func TestIncrementalConservativeToolNameCannotBeRewritten(t *testing.T) {
	RegisterEngine("complete_mask_test", func(json.RawMessage) (GuardrailEngine, error) { return completeMaskEngine{}, nil })
	s := incrementalService()
	s.rules[0].Type = "complete_mask_test"
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"SECRET","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("语义命中工具名称后仍可执行")
	}
	if strings.Contains(out.String(), "tool_calls") {
		t.Fatal("不安全的工具名称已发送")
	}
}

func reassembledChatText(t *testing.T, raw string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			t.Fatal(line)
		}
		for _, choice := range array(event["choices"]) {
			out.WriteString(str(object(object(choice)["delta"])["content"]))
		}
	}
	return out.String()
}

func TestIncrementalMaskClientReassemblyAllSplits(t *testing.T) {
	for _, kind := range []string{"keyword_filter", "credential_detection"} {
		config := []byte(`{"blocklist":["SECRET"],"case_sensitive":true}`)
		text := "安全，SECRET 与 SECRET。"
		if kind == "credential_detection" {
			config = []byte(`{"categories":["api_key"],"mask_keep_prefix":3,"mask_keep_suffix":2}`)
			text = "安全，sk-" + strings.Repeat("Q", 25) + " 后文。"
		}
		engine, err := CreateEngine(kind, config)
		if err != nil {
			t.Fatal(err)
		}
		full, err := engine.Check(t.Context(), text, DirectionResponse, "test")
		if err != nil {
			t.Fatal(err)
		}
		want := applySpans(text, full.MaskedSpans)
		for split := 0; split <= len(text); split++ {
			if !utf8.ValidString(text[:split]) {
				continue
			}
			t.Run(kind+fmt.Sprint(split), func(t *testing.T) {
				s := incrementalService()
				s.rules[0].Type = kind
				s.rules[0].Config = config
				s.rules[0].Action = "mask"
				w, out := incrementalWriter(t, s)
				writeOK(t, w, chatFrame(0, text[:split]))
				writeOK(t, w, chatFrame(0, text[split:]))
				writeOK(t, w, "data: [DONE]\n\n")
				if got := reassembledChatText(t, out.String()); got != want {
					t.Fatalf("客户端重组不同: got=%q want=%q", got, want)
				}
			})
		}
	}
}

func TestIncrementalReviewPreservesDecisionAction(t *testing.T) {
	RegisterEngine("complete_review_test", func(json.RawMessage) (GuardrailEngine, error) { return completeMaskEngine{}, nil })
	s := incrementalService()
	s.rules[0].Type = "complete_review_test"
	s.rules[0].Action = "review"
	r, err := s.NewResponseStream(t.Context(), CheckRequest{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _, err = r.Push(t.Context(), StreamFragment{Key: "text", Text: "SECRET", Final: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Finish(t.Context())
	if err != nil || result == nil || !result.Blocked || result.Action != "review" || len(result.Matched) != 1 || result.Matched[0].Action != "review" {
		t.Fatalf("复核动作被改成普通阻断: %+v %v", result, err)
	}
}

func TestIncrementalPreviewKeepsOnlyTypedPIIPlaceholder(t *testing.T) {
	result := &GuardrailResult{ContentSnippet: "原文13800138000", MaskedSpans: []Span{{Start: 0, End: 11, Replacement: "[PHONE]"}}}
	if got := safeStreamPreview(result); got != "[PHONE]" {
		t.Fatalf("安全占位符预览丢失: %q", got)
	}
}

func TestIncrementalGenericToolMaskCannotExposeJSONKeys(t *testing.T) {
	text := `{"SECRET":"safe"}`
	masked, err := maskJSONDocument(text, []Span{{Start: 0, End: len(text), Replacement: "[REDACTED]"}})
	if err == nil && strings.Contains(masked, "SECRET") {
		t.Fatalf("整范围遮盖仍泄露生成的JSON键名: %s", masked)
	}
}
