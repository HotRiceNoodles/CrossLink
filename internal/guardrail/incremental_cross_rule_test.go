package guardrail

import (
	"context"
	"encoding/json"
	"gorm.io/datatypes"
	"strings"
	"testing"
	"time"
)

func TestIncrementalReviewCrossRuleMaskCannotExposeRetainedSuffix(t *testing.T) {
	s := incrementalService()
	s.rules[0].Type = "credential_detection"
	s.rules[0].Action = "mask"
	s.rules[0].Config = datatypes.JSON(`{"categories":["cloud_credential"]}`)
	second := s.rules[0]
	second.ID = 2
	second.Type = "keyword_filter"
	second.Config = datatypes.JSON(`{"blocklist":["QQQQ!"],"case_sensitive":true}`)
	s.rules = append(s.rules, second)
	w, out := incrementalWriter(t, s)
	secret := "AKIA" + strings.Repeat("Q", 16)
	writeOK(t, w, chatFrame(0, "前："+secret))
	if strings.Contains(out.String(), "QQQQ") {
		t.Fatalf("尚未获关键词规则批准的凭据后缀被 replacement 提前释放: %s", out.String())
	}
	writeOK(t, w, chatFrame(0, "! 结束。"))
	writeOK(t, w, "data: [DONE]\n\n")
}

func TestIncrementalReviewChangedMultipleSnapshotFieldsCheckedTogether(t *testing.T) {
	s := incrementalService()
	s.rules[0].Config = datatypes.JSON(`{"use_regex":true,"blocklist":["SEC\\nRET"],"case_sensitive":true}`)
	w, out := incrementalWriter(t, s)
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "m1", "delta": "safe"}))
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "m2", "delta": "safe"}))
	final := frame("response.completed", map[string]any{"response": map[string]any{"output": []any{
		map[string]any{"id": "m1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "SEC"}}},
		map[string]any{"id": "m2", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "RET"}}},
	}}})
	if _, err := w.Write([]byte(final)); err == nil {
		t.Fatalf("多字段替换快照的组合绕过完整审核: %s", out.String())
	}
}

func TestIncrementalReviewChatRefusalIsInspected(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, `data: {"choices":[{"index":0,"delta":{"refusal":"SECRET"}}]}`+"\n\n")
	writeOK(t, w, "data: [DONE]\n\n")
	if strings.Contains(out.String(), "SECRET") {
		t.Fatalf("Chat refusal 完全跳过审核: %s", out.String())
	}
}

func TestIncrementalReviewMaskDoesNotLeakLogprobs(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, `data: {"choices":[{"index":0,"delta":{"content":"SECRET"},"logprobs":{"content":[{"token":"SECRET","logprob":-0.1,"bytes":[83,69,67,82,69,84],"top_logprobs":[]}]}}]}`+"\n\n")
	writeOK(t, w, "data: [DONE]\n\n")
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "83,69,67,82,69,84") {
		t.Fatalf("delta 脱敏但 logprobs 保留敏感原文/bytes: %s", out.String())
	}
}

func TestIncrementalReviewConservativeSnapshotOnlyToolNameRefuses(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	s.rules[0].Config = datatypes.JSON(`{"use_regex":true,"blocklist":["SECRET"],"case_sensitive":true}`)
	w, out := incrementalWriter(t, s)
	final := frame("response.completed", map[string]any{"response": map[string]any{"output": []any{
		map[string]any{"id": "f1", "type": "function_call", "call_id": "call1", "name": "SECRET", "arguments": "{}"},
	}}})
	if _, err := w.Write([]byte(final)); err == nil {
		t.Fatalf("仅完成快照中的工具名被语义规则遮盖后仍可执行: %s", out.String())
	}
}

func TestIncrementalReviewChangedToolNameSnapshotRefuses(t *testing.T) {
	s := incrementalService()
	s.rules[0].Action = "mask"
	w, out := incrementalWriter(t, s)
	writeOK(t, w, frame("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "f1", "type": "function_call", "call_id": "call1", "name": "lookup", "arguments": ""}}))
	writeOK(t, w, frame("response.function_call_arguments.delta", map[string]any{"item_id": "f1", "output_index": 0, "delta": "{}"}))
	final := frame("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "f1", "type": "function_call", "call_id": "call1", "name": "SECRET", "arguments": "{}"}})
	if _, err := w.Write([]byte(final)); err == nil {
		t.Fatalf("变化工具名快照被mask后仍可执行: %s", out.String())
	}
}

func TestIncrementalReviewLowConfidenceCredentialDoesNotClaimBlock(t *testing.T) {
	s := incrementalService()
	s.rules[0].Type = "credential_detection"
	s.rules[0].Config = datatypes.JSON(`{"categories":["env_file"]}`)
	events := make(chan GuardrailEvent, 4)
	s.AddHandler(func(_ context.Context, event GuardrailEvent) { events <- event })
	w, out := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "PASSWORD=abcdefgh12345678"))
	writeOK(t, w, "data: [DONE]\n\n")
	select {
	case event := <-events:
		if event.Action == "block" {
			t.Fatalf("低置信度凭据原文已放行却记成阻断: event=%+v output=%s", event, out.String())
		}
	case <-time.After(time.Second):
		t.Fatal("没有审计事件")
	}
}

func TestIncrementalReviewLengthSnapshotChecksWholeReplacementAtomically(t *testing.T) {
	s := incrementalService()
	s.rules[0].Type = "content_length"
	s.rules[0].Config = datatypes.JSON(`{"max_output_chars":6}`)
	w, out := incrementalWriter(t, s)
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "m1", "delta": "a"}))
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "m2", "delta": "bbbb"}))
	final := frame("response.completed", map[string]any{"response": map[string]any{"output": []any{
		map[string]any{"id": "m1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "cccc"}}},
		map[string]any{"id": "m2", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "d"}}},
	}}})
	if _, err := w.Write([]byte(final)); err != nil {
		t.Fatalf("同帧完整快照总量仍6字符却被中间替换状态误拦截: %v; %s", err, out.String())
	}
}

type reviewIncompleteEngine struct{}

func (reviewIncompleteEngine) Name() string { return "review_incomplete" }
func (reviewIncompleteEngine) Check(ctx context.Context, _ string, _ Direction, _ string) (*GuardrailResult, error) {
	cc, _ := ctx.Value(CtxKeyCheck).(*CheckContext)
	cc.Metadata["guardrail_audit_status"] = "incomplete"
	cc.Metadata["guardrail_error_reason"] = "engine_unavailable"
	return &GuardrailResult{Blocked: false, Reason: "检测不完整，规则层未命中"}, nil
}
func TestIncrementalReviewIncompleteAuditDoesNotCreatePolicyMatch(t *testing.T) {
	RegisterEngine("review_incomplete", func(_ json.RawMessage) (GuardrailEngine, error) { return reviewIncompleteEngine{}, nil })
	s := incrementalService()
	s.rules[0].Type = "review_incomplete"
	events := make(chan GuardrailEvent, 4)
	s.AddHandler(func(_ context.Context, event GuardrailEvent) { events <- event })
	w, _ := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "safe"))
	writeOK(t, w, "data: [DONE]\n\n")
	deadline := time.NewTimer(300 * time.Millisecond)
	defer deadline.Stop()
	count := 0
	for {
		select {
		case event := <-events:
			count++
			if event.Reason == "policy_match" {
				t.Fatalf("审核错误且未命中仍产生policy_match事件: %+v", event)
			}
		case <-deadline.C:
			if count == 0 {
				t.Fatal("没有审核错误事件")
			}
			return
		}
	}
}

func TestIncrementalSnapshotReturningToOriginalClearsCombinedState(t *testing.T) {
	s := incrementalService()
	s.rules[0].Config = datatypes.JSON(`{"use_regex":true,"blocklist":["SEC\\nRET"],"case_sensitive":true}`)
	w, _ := incrementalWriter(t, s)
	for _, id := range []string{"m1", "m2"} {
		writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": id, "delta": "safe"}))
	}
	for _, change := range []struct{ id, text string }{{"m1", "SEC"}, {"m1", "safe"}, {"m2", "RET"}} {
		writeOK(t, w, frame("response.output_text.done", map[string]any{"item_id": change.id, "text": change.text}))
	}
	writeOK(t, w, "data: [DONE]\n\n")
}
