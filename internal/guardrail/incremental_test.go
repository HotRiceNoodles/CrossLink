package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
)

func incrementalService() *GuardrailService {
	s := NewGuardrailService(nil, nil)
	s.SetEnabled(true)
	s.SetStreamMode("incremental")
	s.loadedAt = time.Now()
	s.modelCfgs["test"] = modelCfgEntry{loaded: time.Now()}
	s.rules = []GuardrailRule{{ID: 1, Version: 3, Enabled: true, Name: "secrets", Type: "keyword_filter", Direction: "response", Action: "block", Severity: "high", Config: datatypes.JSON(`{"blocklist":["SECRET"],"case_sensitive":true}`)}}
	return s
}
func incrementalWriter(t *testing.T, s *GuardrailService) (*IncrementalSSEWriter, *bytes.Buffer) {
	t.Helper()
	stream, err := s.NewResponseStream(context.Background(), CheckRequest{Model: "test", OrgID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if stream.Fallback {
		t.Fatal("unexpected strict fallback")
	}
	out := &bytes.Buffer{}
	w := NewIncrementalSSEWriter(context.Background(), stream, out, nil)
	t.Cleanup(func() { _ = w.Close() })
	return w, out
}
func chatFrame(index int, text string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": index, "delta": map[string]any{"content": text}}}})
	return "data: " + string(raw) + "\n\n"
}
func frame(typ string, obj any) string {
	raw, _ := json.Marshal(obj)
	return "event: " + typ + "\ndata: " + string(raw) + "\n\n"
}
func writeOK(t *testing.T, w *IncrementalSSEWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalReleasesBeforeGenerationEnds(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	first := chatFrame(0, "hello")
	writeOK(t, w, first)
	if out.String() != first {
		t.Fatalf("first delta was batched: %q", out.String())
	}
	writeOK(t, w, chatFrame(0, " world"))
	writeOK(t, w, "data: [DONE]\n\n")
	if !strings.HasSuffix(out.String(), "data: [DONE]\n\n") {
		t.Fatal(out.String())
	}
}

func TestIncrementalRetainsOnlyCandidateAndPreservesEvents(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	candidate := chatFrame(0, "SE")
	writeOK(t, w, candidate)
	if out.Len() != 0 {
		t.Fatal("candidate leaked")
	}
	next := chatFrame(0, "X")
	writeOK(t, w, next)
	if out.String() != candidate+next {
		t.Fatalf("original event cadence/framing changed: %s", out.String())
	}
}

func TestIncrementalSplitsApprovedTextPrefix(t *testing.T) {
	for _, protocol := range []string{"chat", "chat-reasoning", "anthropic", "anthropic-thinking", "responses", "responses-reasoning", "responses-summary", "responses-refusal"} {
		t.Run(protocol, func(t *testing.T) {
			w, out := incrementalWriter(t, incrementalService())
			flushes := 0
			w.flush = func() { flushes++ }
			delta := func(text string, sequence int) string {
				if strings.HasPrefix(protocol, "responses") {
					typ := map[string]string{"responses": "response.output_text.delta", "responses-reasoning": "response.reasoning_text.delta", "responses-summary": "response.reasoning_summary_text.delta", "responses-refusal": "response.refusal.delta"}[protocol]
					return frame(typ, map[string]any{"item_id": "msg1", "content_index": 0, "sequence_number": sequence, "delta": text})
				}
				if strings.HasPrefix(protocol, "anthropic") {
					field, typ := "text", "text_delta"
					if protocol == "anthropic-thinking" {
						field, typ = "thinking", "thinking_delta"
					}
					return frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": typ, field: text}})
				}
				field := "content"
				if protocol == "chat-reasoning" {
					field = "reasoning_content"
				}
				payload, _ := json.Marshal(map[string]any{"id": "completion1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{field: text}}}})
				return "data: " + string(payload) + "\n\n"
			}
			first := strings.ReplaceAll("id: event1\n"+delta("你好，SE", 10), "\n", "\r\n")
			writeOK(t, w, first)
			if !strings.Contains(out.String(), "你好，") || strings.Contains(out.String(), "SE") || strings.Contains(out.String(), "id: event1") || flushes != 1 {
				t.Fatalf("安全前文未立即单独放行：%q, flushes=%d", out.String(), flushes)
			}
			writeOK(t, w, delta("X，完成。", 11))
			writeOK(t, w, "data: [DONE]\n\n")
			var combined strings.Builder
			var sequences []int
			for _, line := range strings.Split(strings.ReplaceAll(out.String(), "\r\n", "\n"), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var obj map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
					t.Fatal(err)
				}
				if sequence, ok := obj["sequence_number"].(float64); ok {
					sequences = append(sequences, int(sequence))
				}
				if strings.HasPrefix(protocol, "responses") {
					combined.WriteString(str(obj["delta"]))
				} else if strings.HasPrefix(protocol, "anthropic") {
					d := object(obj["delta"])
					combined.WriteString(str(d["text"]) + str(d["thinking"]))
				} else {
					d := object(object(array(obj["choices"])[0])["delta"])
					combined.WriteString(str(d["content"]) + str(d["reasoning_content"]))
				}
			}
			if combined.String() != "你好，SEX，完成。" || w.bytes != 0 || len(w.pending) != 0 {
				t.Fatalf("拆分丢失或重复文本：%q pending=%d bytes=%d", combined.String(), len(w.pending), w.bytes)
			}
			if strings.HasPrefix(protocol, "responses") && fmt.Sprint(sequences) != "[10 11 12]" {
				t.Fatalf("Responses 序号未保持递增：%v", sequences)
			}
			if strings.Count(out.String(), "id: event1") != 1 {
				t.Fatal("原始事件 ID 未在剩余片段中保留一次")
			}
		})
	}
}

func TestIncrementalSplitPrefixDropsCandidateOnBlockOrEOF(t *testing.T) {
	for _, ending := range []string{"block", "eof"} {
		t.Run(ending, func(t *testing.T) {
			w, out := incrementalWriter(t, incrementalService())
			writeOK(t, w, chatFrame(0, "安全前文SE"))
			if !strings.Contains(out.String(), "安全前文") || strings.Contains(out.String(), "SE") {
				t.Fatal(out.String())
			}
			if ending == "block" {
				if _, err := w.Write([]byte(chatFrame(0, "CRET后文"))); !errors.Is(err, ErrStreamBlocked) {
					t.Fatal(err)
				}
			} else if w.Close() == nil {
				t.Fatal("异常 EOF 放行候选前缀")
			}
			if strings.Contains(out.String(), "SE") || strings.Contains(out.String(), "后文") || !strings.Contains(out.String(), "guardrail_blocked") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestIncrementalSplitPreservesNumbersAndCompositeEvents(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	writeOK(t, w, `data: {"created":9007199254740993,"choices":[{"index":0,"delta":{"content":"安全SE"}}]}`+"\n\n")
	if !strings.Contains(out.String(), "9007199254740993") || strings.Contains(out.String(), "SE") {
		t.Fatal(out.String())
	}
	for _, raw := range []string{
		`data: {"usage":{"completion_tokens":1},"choices":[{"index":0,"delta":{"content":"安全SE"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"安全SE"},"logprobs":{"content":[]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"安全SE"}},{"index":1,"delta":{},"finish_reason":"stop"}]}`,
	} {
		w, out := incrementalWriter(t, incrementalService())
		writeOK(t, w, raw+"\n\n")
		if out.Len() != 0 {
			t.Fatalf("包含统计、对数概率或多个 choice 的事件被拆分：%s", out.String())
		}
	}
}

func TestIncrementalBlocksSplitInterleavedChoices(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	writeOK(t, w, chatFrame(0, "SE"))
	writeOK(t, w, chatFrame(1, "other"))
	if _, err := w.Write([]byte(chatFrame(0, "CRET"))); !errors.Is(err, ErrStreamBlocked) {
		t.Fatalf("wanted block, got %v", err)
	}
	if strings.Contains(out.String(), `"SE"`) || strings.Contains(out.String(), `"CRET"`) {
		t.Fatalf("sensitive fragment leaked: %s", out.String())
	}
	if w.Result == nil || w.Result.RuleName != "secrets" {
		t.Fatal("missing attribution")
	}
}

func TestIncrementalUnicodeOffsetsAndCaseFolding(t *testing.T) {
	s := incrementalService()
	s.rules[0].Config = datatypes.JSON(`{"blocklist":["k秘密"]}`)
	w, out := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "你好"))
	before := out.Len()
	writeOK(t, w, chatFrame(0, "K秘"))
	if out.Len() != before {
		t.Fatal("Unicode candidate leaked")
	}
	if _, err := w.Write([]byte(chatFrame(0, "密"))); !errors.Is(err, ErrStreamBlocked) {
		t.Fatal(err)
	}
}

func TestIncrementalToolArgumentsAreWithheldAndDecoded(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	tool := func(args, name string) string {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": name, "arguments": args}}}}}}})
		return "data: " + string(raw) + "\n\n"
	}
	writeOK(t, w, chatFrame(0, "hello"))
	safe := out.String()
	writeOK(t, w, tool(`{"value":"\u0053`, "send"))
	writeOK(t, w, tool(`ECRET"}`, ""))
	if out.String() != safe {
		t.Fatal("partial executable tool call escaped")
	}
	_, err := w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
	if !errors.Is(err, ErrStreamBlocked) {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "tool_calls") {
		t.Fatalf("tool call leaked: %s", out.String())
	}
}

func TestIncrementalAnthropicToolAndThinking(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	start := frame("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": "lookup", "input": map[string]any{}}})
	writeOK(t, w, start)
	if out.Len() != 0 {
		t.Fatal("tool start escaped")
	}
	stop := frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeOK(t, w, stop)
	if out.String() != start+stop {
		t.Fatal(out.String())
	}
	writeOK(t, w, frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "thinking_delta", "thinking": "SE"}}))
	_, err := w.Write([]byte(frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "thinking_delta", "thinking": "CRET"}})))
	if !errors.Is(err, ErrStreamBlocked) {
		t.Fatal(err)
	}
}

func TestIncrementalResponsesSnapshotsAndDoneMarker(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	delta := frame("response.output_text.delta", map[string]any{"item_id": "msg1", "content_index": 0, "delta": "hello"})
	writeOK(t, w, delta)
	if out.String() != delta {
		t.Fatal("delta not released")
	}
	done := frame("response.output_text.done", map[string]any{"item_id": "msg1", "content_index": 0, "text": "hello"})
	writeOK(t, w, done)
	complete := frame("response.completed", map[string]any{"response": map[string]any{"output": []any{map[string]any{"id": "msg1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}}}})
	writeOK(t, w, complete)
	writeOK(t, w, "data: [DONE]\n")
	writeOK(t, w, "\n")
	if out.String() != delta+done+complete+"data: [DONE]\n\n" {
		t.Fatal(out.String())
	}
}

func TestIncrementalResponsesTerminalOnlyContentBlocked(t *testing.T) {
	for _, typ := range []string{"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"} {
		t.Run(typ, func(t *testing.T) {
			w, out := incrementalWriter(t, incrementalService())
			obj := map[string]any{"item_id": "x", "text": "SECRET"}
			switch typ {
			case "response.content_part.done":
				obj = map[string]any{"part": map[string]any{"type": "output_text", "text": "SECRET"}}
			case "response.output_item.done":
				obj = map[string]any{"item": map[string]any{"id": "x", "content": []any{map[string]any{"text": "SECRET"}}}}
			case "response.completed":
				obj = map[string]any{"response": map[string]any{"output": []any{map[string]any{"id": "x", "content": []any{map[string]any{"text": "SECRET"}}}}}}
			}
			_, err := w.Write([]byte(frame(typ, obj)))
			if !errors.Is(err, ErrStreamBlocked) {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "SECRET") {
				t.Fatal("terminal payload leaked")
			}
		})
	}
}

func TestIncrementalFallsBackForUnsupportedPolicies(t *testing.T) {
	RegisterEngine("incremental_legacy_test", func(raw json.RawMessage) (GuardrailEngine, error) {
		engine, err := NewLengthEngineFromConfig(raw)
		return struct{ GuardrailEngine }{engine}, err
	})
	for _, kind := range []string{"regex", "mask", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			s := incrementalService()
			switch kind {
			case "regex":
				s.rules[0].Config = datatypes.JSON(`{"blocklist":["SEC.*RET"],"use_regex":true}`)
			case "mask":
				s.rules[0].Action = "mask"
			case "legacy":
				s.rules[0].Type = "incremental_legacy_test"
				s.rules[0].Config = datatypes.JSON(`{}`)
			}
			r, err := s.NewResponseStream(context.Background(), CheckRequest{Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if !r.Fallback {
				t.Fatal("unsupported policy treated as incremental")
			}
		})
	}
}

func TestIncrementalObservationRulesDoNotForceBatching(t *testing.T) {
	s := incrementalService()
	s.rules = append(s.rules, GuardrailRule{ID: 2, Enabled: true, Name: "observe", Type: "content_length", Direction: "both", Action: "log", Config: datatypes.JSON(`{}`)})
	w, out := incrementalWriter(t, s)
	writeOK(t, w, chatFrame(0, "hello"))
	if out.Len() == 0 {
		t.Fatal("observation rule blocked release")
	}
}

func TestIncrementalEOFAndCancellationDropUnapprovedTail(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	writeOK(t, w, chatFrame(0, "SE"))
	if w.Close() == nil {
		t.Fatal("expected truncated tail error")
	}
	if strings.Contains(out.String(), `"SE"`) {
		t.Fatal("tail escaped at EOF")
	}
	s := incrementalService()
	r, err := s.NewResponseStream(context.Background(), CheckRequest{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out.Reset()
	w = NewIncrementalSSEWriter(ctx, r, out, nil)
	writeOK(t, w, chatFrame(0, "SE"))
	cancel()
	if !errors.Is(w.Close(), context.Canceled) || out.Len() != 0 {
		t.Fatal("cancellation released pending data")
	}
}

func TestIncrementalFramingAndQueueLimits(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	raw := strings.ReplaceAll(chatFrame(0, "你好"), "\n", "\r\n")
	for _, b := range []byte(raw) {
		writeOK(t, w, string([]byte{b}))
	}
	if out.String() != raw {
		t.Fatal("fragmented UTF-8 or CRLF altered")
	}
	s := incrementalService()
	s.strictMaxBytes.Store(64)
	w, out = incrementalWriter(t, s)
	if _, err := w.Write([]byte("data: " + strings.Repeat("x", 65))); err == nil {
		t.Fatal("byte cap ignored")
	}
	if strings.Contains(out.String(), strings.Repeat("x", 65)) {
		t.Fatal("unparsed data escaped")
	}
}

type waitingStreamEngine struct{}

func (waitingStreamEngine) Name() string { return "waiting" }
func (waitingStreamEngine) Check(context.Context, string, Direction, string) (*GuardrailResult, error) {
	return &GuardrailResult{}, nil
}
func (waitingStreamEngine) NewStream(context.Context, CheckRequest) (StreamSession, error) {
	return waitingStream{}, nil
}

type waitingStream struct{}

func (waitingStream) Push(ctx context.Context, _ StreamFragment) (StreamVerdict, error) {
	<-ctx.Done()
	return StreamVerdict{}, ctx.Err()
}
func (waitingStream) Close() {}

func TestIncrementalTimeoutFailsClosed(t *testing.T) {
	RegisterEngine("incremental_wait_test", func(json.RawMessage) (GuardrailEngine, error) { return waitingStreamEngine{}, nil })
	s := incrementalService()
	s.rules[0].Type = "incremental_wait_test"
	s.responseBudgetMs.Store(5)
	w, out := incrementalWriter(t, s)
	start := time.Now()
	_, err := w.Write([]byte(chatFrame(0, "hello")))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hello") {
		t.Fatal("timeout released unchecked text")
	}
}

func BenchmarkIncrementalKeywordStream(b *testing.B) {
	engine, _ := NewKeywordEngineFromConfig(json.RawMessage(`{"blocklist":["SECRET"]}`))
	session, _ := engine.(StreamingEngine).NewStream(context.Background(), CheckRequest{})
	defer session.Close()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := session.Push(context.Background(), StreamFragment{Key: "text", Text: "正常文本"})
		if err != nil {
			b.Fatal(fmt.Sprint(err))
		}
	}
}

func TestIncrementalSessionsAreIsolatedAndRulesScoped(t *testing.T) {
	s := incrementalService()
	w1, _ := incrementalWriter(t, s)
	w2, out2 := incrementalWriter(t, s)
	writeOK(t, w1, chatFrame(0, "SE"))
	writeOK(t, w2, chatFrame(0, "CRET"))
	if out2.Len() == 0 {
		t.Fatal("independent stream inherited another stream's prefix")
	}
	org := int64(99)
	s.rules[0].OrgID = &org
	r, err := s.NewResponseStream(context.Background(), CheckRequest{Model: "test", OrgID: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.rules) != 0 {
		t.Fatal("foreign organization rule selected")
	}
	s.rules[0].OrgID = nil
	filter := "other"
	s.rules[0].ModelFilter = &filter
	r2, err := s.NewResponseStream(context.Background(), CheckRequest{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if len(r2.rules) != 0 {
		t.Fatal("nonmatching model rule selected")
	}
}

type leasedStreamEngine struct{ closed *atomic.Int32 }

func (e leasedStreamEngine) Name() string { return "leased_test" }
func (e leasedStreamEngine) Check(context.Context, string, Direction, string) (*GuardrailResult, error) {
	return &GuardrailResult{}, nil
}
func (e leasedStreamEngine) Close() { e.closed.Add(1) }

func TestIncrementalEngineLeaseSurvivesReload(t *testing.T) {
	var created, closed atomic.Int32
	RegisterEngine("incremental_lease_test", func(json.RawMessage) (GuardrailEngine, error) {
		created.Add(1)
		return leasedStreamEngine{&closed}, nil
	})
	s := incrementalService()
	rule := s.rules[0]
	rule.Type = "incremental_lease_test"
	_, release1, err := s.leaseStreamEngine(rule)
	if err != nil {
		t.Fatal(err)
	}
	_, release2, err := s.leaseStreamEngine(rule)
	if err != nil {
		t.Fatal(err)
	}
	if created.Load() != 1 {
		t.Fatal("engine recompiled per response")
	}
	s.pruneStreamEngines(nil)
	release1()
	if closed.Load() != 0 {
		t.Fatal("live response lost its engine")
	}
	release2()
	if closed.Load() != 1 {
		t.Fatal("retired engine not released exactly once")
	}
}

func TestIncrementalRefusalAndInterruptedTools(t *testing.T) {
	w, out := incrementalWriter(t, incrementalService())
	writeOK(t, w, frame("response.refusal.delta", map[string]any{"item_id": "r1", "delta": "SE"}))
	if _, err := w.Write([]byte(frame("response.refusal.delta", map[string]any{"item_id": "r1", "delta": "CRET"}))); !errors.Is(err, ErrStreamBlocked) {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("refusal content bypassed moderation")
	}
	w, out = incrementalWriter(t, incrementalService())
	writeOK(t, w, frame("response.function_call_arguments.delta", map[string]any{"item_id": "t1", "delta": "{}"}))
	if _, err := w.Write([]byte(frame("response.incomplete", map[string]any{"response": map[string]any{"output": []any{}}}))); err == nil {
		t.Fatal("interrupted tool released")
	}
	if strings.Contains(out.String(), "arguments") {
		t.Fatal("tool payload escaped")
	}
}
