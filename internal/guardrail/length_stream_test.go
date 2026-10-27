package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"gorm.io/datatypes"
)

func lengthSession(t testing.TB, limit int) StreamSession {
	t.Helper()
	raw, _ := json.Marshal(lengthConfig{MaxInputChars: 1, MaxOutputChars: limit})
	engine, err := NewLengthEngineFromConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.(StreamingEngine).NewStream(context.Background(), CheckRequest{Content: strings.Repeat("请求", 20)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	return session
}

func TestIncrementalLengthToolUsesDecodedJSONAndAggregateBudget(t *testing.T) {
	canonical := `{"value":"你好"}`
	count := utf8.RuneCountInString(canonical)
	for _, withText := range []bool{false, true} {
		svc := incrementalService()
		svc.rules[0].Type = "content_length"
		limit := count
		if withText {
			limit += 2 // 两字正文加字段分隔符后应超限一个字符。
		}
		cfg, _ := json.Marshal(lengthConfig{MaxOutputChars: limit})
		svc.rules[0].Config = datatypes.JSON(cfg)
		w, out := incrementalWriter(t, svc)
		if withText {
			writeOK(t, w, chatFrame(0, "正文"))
		}
		encoded := `{"value":"\u4f60\u597d"}`
		for _, part := range []string{encoded[:14], encoded[14:]} {
			raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": part}}}}}}})
			writeOK(t, w, "data: "+string(raw)+"\n\n")
		}
		_, err := w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
		if withText {
			if !errors.Is(err, ErrStreamBlocked) || strings.Contains(out.String(), "arguments") {
				t.Fatal("工具参数绕过总长度预算", err, out.String())
			}
		} else if err != nil || !strings.Contains(out.String(), "arguments") {
			t.Fatal("Unicode 转义被按原始字符重复计数", err, out.String())
		} else {
			writeOK(t, w, "data: [DONE]\n\n")
		}
	}
}

func BenchmarkIncrementalLength(b *testing.B) {
	s := lengthSession(b, -1)
	b.ReportAllocs()
	for range b.N {
		if _, err := s.Push(context.Background(), StreamFragment{Text: "普通文字持续输出。"}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestIncrementalLengthUnicodeAndImmediateRelease(t *testing.T) {
	s := lengthSession(t, 4)
	total := 0
	for _, text := range []string{"你🙂", "好a"} {
		total += len(text)
		v, err := s.Push(t.Context(), StreamFragment{Key: "text", Text: text})
		if err != nil || v.Result != nil || v.ReleaseThrough != total || v.CheckedThrough != total {
			t.Fatalf("正常文本被暂留或 Unicode 计数有误：%+v %v", v, err)
		}
	}
	v, err := s.Push(t.Context(), StreamFragment{Key: "text", Text: "b"})
	if err != nil || v.Result == nil || !v.Result.Blocked || v.Result.Reason != "output length 5 exceeds max 4 characters" || v.Result.ContentSnippet == "" {
		t.Fatalf("未在超过上限时截停：%+v %v", v, err)
	}
}

func TestIncrementalLengthAggregatesFieldsAndSessionsArePrivate(t *testing.T) {
	s := lengthSession(t, 5)
	for _, f := range []StreamFragment{{Key: "one", Text: "ab"}, {Key: "two", Text: "你🙂"}, {Key: "one", Final: true}} {
		v, err := s.Push(t.Context(), f)
		if err != nil || v.Result != nil {
			t.Fatalf("字段分隔或完成信号重复计数：%+v %v", v, err)
		}
	}
	v, err := s.Push(t.Context(), StreamFragment{Key: "two", Text: "c"})
	if err != nil || v.Result == nil || !v.Result.Blocked {
		t.Fatalf("分字段绕过总长度：%+v %v", v, err)
	}
	other := lengthSession(t, 5)
	v, err = other.Push(t.Context(), StreamFragment{Text: "abcde", Final: true})
	if err != nil || v.Result != nil || v.ReleaseThrough != 5 {
		t.Fatalf("跨响应累计：%+v %v", v, err)
	}
}

func TestIncrementalLengthDisabledLimitAndLifecycle(t *testing.T) {
	s := lengthSession(t, -1)
	v, err := s.Push(t.Context(), StreamFragment{Text: strings.Repeat("字", 100)})
	if err != nil || v.Result != nil || v.ReleaseThrough != 300 {
		t.Fatal(v, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Push(ctx, StreamFragment{Text: "a"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Push(t.Context(), StreamFragment{Text: string([]byte{0xff})}); err == nil {
		t.Fatal("接受非法 UTF-8")
	}
	s.Close()
	if _, err := s.Push(t.Context(), StreamFragment{Text: "a"}); err == nil {
		t.Fatal("关闭后仍接受增量")
	}
}

func TestIncrementalLengthSSEAndSnapshotAccounting(t *testing.T) {
	svc := incrementalService()
	svc.rules[0].Type = "content_length"
	svc.rules[0].Config = datatypes.JSON(`{"max_output_chars":5}`)
	w, out := incrementalWriter(t, svc)
	writeOK(t, w, chatFrame(0, "hello"))
	if !strings.Contains(out.String(), "hello") {
		t.Fatal("安全文本没有立即放行")
	}
	if _, err := w.Write([]byte(chatFrame(1, "x"))); !errors.Is(err, ErrStreamBlocked) {
		t.Fatalf("多个 choice 绕过总长度：%v", err)
	}
	for _, replacement := range []bool{false, true} {
		svc := incrementalService()
		svc.rules[0].Type = "content_length"
		svc.rules[0].Config = datatypes.JSON(`{"max_output_chars":5}`)
		w, out := incrementalWriter(t, svc)
		delta := func(id, text string) string {
			return frame("response.output_text.delta", map[string]any{"item_id": id, "content_index": 0, "delta": text})
		}
		writeOK(t, w, delta("first", "ab"))
		writeOK(t, w, delta("second", "cd"))
		text := "ab"
		if replacement {
			text = "abc"
		}
		_, err := w.Write([]byte(frame("response.output_text.done", map[string]any{"item_id": "first", "content_index": 0, "text": text})))
		if replacement {
			if !errors.Is(err, ErrStreamBlocked) || strings.Contains(out.String(), `"text":"abc"`) {
				t.Fatalf("替换快照绕过总长度：%v %s", err, out.String())
			}
		} else {
			if err != nil {
				t.Fatalf("重复快照被重复计数：%v", err)
			}
			writeOK(t, w, "data: [DONE]\n\n")
		}
	}
}

func TestIncrementalLengthSnapshotReturningToOriginalText(t *testing.T) {
	svc := incrementalService()
	svc.rules[0].Type = "content_length"
	svc.rules[0].Config = datatypes.JSON(`{"max_output_chars":6}`)
	w, out := incrementalWriter(t, svc)
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "first", "delta": "abcd"}))
	writeOK(t, w, frame("response.output_text.done", map[string]any{"item_id": "first", "text": "ab"}))
	writeOK(t, w, frame("response.output_text.delta", map[string]any{"item_id": "second", "delta": "cde"}))
	_, err := w.Write([]byte(frame("response.output_text.done", map[string]any{"item_id": "first", "text": "abcd"})))
	if !errors.Is(err, ErrStreamBlocked) || strings.Contains(out.String(), `"text":"abcd"`) {
		t.Fatal("恢复原文的快照没有重新计长", err, out.String())
	}
}
