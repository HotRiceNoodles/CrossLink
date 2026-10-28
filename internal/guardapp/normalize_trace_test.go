package guardapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

// 协议抽取语料沿用 middleware/guardrail 现有测试的形态（tool JSON 转义、
// 多模态标记），确保共享抽取层在 guard 规范化路径下行为一致。

func TestNormalizeContentPlainString(t *testing.T) {
	for _, p := range []string{ProtocolOpenAI, ProtocolAnthropic, ProtocolGeneric} {
		text, uninsp, ok := NormalizeContent(json.RawMessage(`"hello SECRET"`), p)
		if !ok || text != "hello SECRET" || uninsp {
			t.Fatalf("protocol %s: text=%q ok=%v uninsp=%v", p, text, ok, uninsp)
		}
	}
}

func TestNormalizeContentOpenAIBody(t *testing.T) {
	// 完整请求体：tool_calls 参数里的 C 转义必须解码后参与检测
	body := `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"SECRET in tool arg"},` +
		`{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":"{\"cmd\":\"run SE\\u0043RET wipe\"}"}}]}` +
		`]}`
	text, _, ok := NormalizeContent(json.RawMessage(body), ProtocolOpenAI)
	if !ok {
		t.Fatal("openai body should normalize")
	}
	if !strings.Contains(text, "SECRET in tool arg") || !strings.Contains(text, `run SECRET wipe`) {
		t.Fatalf("decoded escapes missing from detection text: %q", text)
	}
}

func TestNormalizeContentMessagesArray(t *testing.T) {
	// messages 数组：包一层 {"messages": content} 再抽取
	arr := `[{"role":"user","content":"hello"},{"role":"assistant","content":"world"}]`
	text, _, ok := NormalizeContent(json.RawMessage(arr), ProtocolAnthropic)
	if !ok || text != "hello\nworld" {
		t.Fatalf("anthropic array: text=%q ok=%v", text, ok)
	}
}

func TestNormalizeContentUninspectable(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"safe"},{"type":"image_url","image_url":{"url":"https://x/1.png"}}]}]}`
	text, uninsp, ok := NormalizeContent(json.RawMessage(body), ProtocolOpenAI)
	if !ok || !uninsp {
		t.Fatalf("image part must mark uninspectable: text=%q uninsp=%v ok=%v", text, uninsp, ok)
	}
	if text != "safe" {
		t.Fatalf("text parts still extracted: %q", text)
	}
}

func TestNormalizeContentGeneric(t *testing.T) {
	text, _, ok := NormalizeContent(json.RawMessage(`{"content":"generic body","other":1}`), ProtocolGeneric)
	if !ok || text != "generic body" {
		t.Fatalf("generic object: text=%q ok=%v", text, ok)
	}
	if _, _, ok := NormalizeContent(json.RawMessage(`{"no_content":1}`), ProtocolGeneric); ok {
		t.Fatal("generic without content string must be ok=false")
	}
}

func TestNormalizeContentInvalid(t *testing.T) {
	cases := []json.RawMessage{[]byte(``), []byte(`""`), []byte(`{}`), []byte(`123`), []byte(`true`)}
	for _, c := range cases {
		if _, _, ok := NormalizeContent(c, ProtocolOpenAI); ok {
			t.Fatalf("content %s must not normalize", c)
		}
	}
	// generic 数组不支持
	if _, _, ok := NormalizeContent(json.RawMessage(`["a"]`), ProtocolGeneric); ok {
		t.Fatal("generic array must not normalize")
	}
}

func TestMetadataMap(t *testing.T) {
	m := MetadataMap(json.RawMessage(`{"api_key_id":"42","user":"u1"}`))
	if m == nil || m["api_key_id"] != "42" || m["user"] != "u1" {
		t.Fatalf("metadata map: %v", m)
	}
	if MetadataMap(json.RawMessage(`"not-object"`)) != nil {
		t.Fatal("non-object metadata must return nil")
	}
	if MetadataMap(nil) != nil {
		t.Fatal("nil metadata must return nil")
	}
}

func newTraceStore() *TraceStore {
	s := NewTraceStore(15*time.Minute, 3)
	return s
}

func TestTraceStorePairingAndOrgIsolation(t *testing.T) {
	s := newTraceStore()
	s.SavePrompt(1, "tr-1", "request text")
	if p, ok := s.TakePrompt(1, "tr-1"); !ok || p != "request text" {
		t.Fatalf("take: p=%q ok=%v", p, ok)
	}
	if _, ok := s.TakePrompt(1, "tr-1"); ok {
		t.Fatal("take must be one-shot")
	}
	// org 隔离：同 trace_id 不同 org 互不可见
	s.SavePrompt(1, "tr-2", "org-one")
	s.SavePrompt(2, "tr-2", "org-two")
	if p, _ := s.TakePrompt(2, "tr-2"); p != "org-two" {
		t.Fatalf("org isolation broken: %q", p)
	}
	if p, _ := s.TakePrompt(1, "tr-2"); p != "org-one" {
		t.Fatalf("org isolation broken: %q", p)
	}
}

func TestTraceStoreTTLExpiry(t *testing.T) {
	s := newTraceStore()
	base := time.Now()
	s.now = func() time.Time { return base }
	s.SavePrompt(1, "tr", "text")
	// 窗口内取回正常
	if _, ok := s.TakePrompt(1, "tr"); !ok {
		t.Fatal("inside TTL must hit")
	}
	s.SavePrompt(1, "tr", "text")
	s.now = func() time.Time { return base.Add(s.ttl + time.Second) }
	if _, ok := s.TakePrompt(1, "tr"); ok {
		t.Fatal("expired entry must miss")
	}
	ttlEv, lruEv := s.Evicted()
	if ttlEv != 1 || lruEv != 0 {
		t.Fatalf("eviction counters: ttl=%d lru=%d", ttlEv, lruEv)
	}
}

func TestTraceStoreLRUCap(t *testing.T) {
	s := newTraceStore() // maxEntries=3
	s.SavePrompt(1, "a", "a")
	s.SavePrompt(1, "b", "b")
	s.SavePrompt(1, "c", "c")
	// 触碰 a 使 b 成为 LRU 尾部
	s.SavePrompt(1, "a", "a2")
	s.SavePrompt(1, "d", "d") // 淘汰 b
	if _, ok := s.TakePrompt(1, "b"); ok {
		t.Fatal("LRU tail must be evicted")
	}
	if _, ok := s.TakePrompt(1, "a"); !ok {
		t.Fatal("recently used must survive")
	}
	_, lruEv := s.Evicted()
	if lruEv != 1 {
		t.Fatalf("lru evictions: %d", lruEv)
	}
}

func TestVerdictFromCheck(t *testing.T) {
	if v := VerdictFromCheck(nil, nil); v != model.GuardVerdictPass {
		t.Fatalf("nil result: %s", v)
	}
	if v := VerdictFromCheck(nil, context.Canceled); v != model.GuardVerdictError {
		t.Fatalf("error: %s", v)
	}
	if v := VerdictFromCheck(&guardrail.CheckResponse{Blocked: true}, nil); v != model.GuardVerdictWouldBlock {
		t.Fatalf("blocked: %s", v)
	}
	if v := VerdictFromCheck(&guardrail.CheckResponse{Action: "mask"}, nil); v != model.GuardVerdictFlagged {
		t.Fatalf("mask: %s", v)
	}
	if v := VerdictFromCheck(&guardrail.CheckResponse{Action: ""}, nil); v != model.GuardVerdictPass {
		t.Fatalf("pass: %s", v)
	}
}

func TestTraceRecorderUpsert(t *testing.T) {
	db, _, _, _ := newTestApp(t) // 真实迁移链建库（含 guard 三表）
	repo := repository.NewGuardTraceRepo(db)
	store := NewTraceStore(time.Minute, 10)
	rec := NewTraceRecorder(repo, store)
	ctx := context.Background()

	// 请求向：trace 建行，verdict=flagged
	if err := rec.RecordRequest(ctx, 7, "tr-1", "the prompt", model.GuardVerdictFlagged); err != nil {
		t.Fatal(err)
	}
	if p, ok := store.TakePrompt(7, "tr-1"); !ok || p != "the prompt" {
		t.Fatalf("prompt not stored: %q %v", p, ok)
	}
	// 响应向 would_block 覆盖 flagged，双向完成 → complete
	if err := rec.RecordResponse(ctx, 7, "tr-1", model.GuardVerdictWouldBlock); err != nil {
		t.Fatal(err)
	}
	tr, err := repo.FindByOrgTrace(ctx, 7, "tr-1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != model.GuardTraceComplete || tr.Verdict != model.GuardVerdictWouldBlock ||
		tr.RequestAt == nil || tr.ResponseAt == nil {
		t.Fatalf("trace: %+v", tr)
	}

	// 响应先到（响应向独立提交）：建行 status=open，请求后到再 complete
	if err := rec.RecordResponse(ctx, 8, "tr-2", model.GuardVerdictPass); err != nil {
		t.Fatal(err)
	}
	tr2, _ := repo.FindByOrgTrace(ctx, 8, "tr-2")
	if tr2.Status != model.GuardTraceOpen || tr2.RequestAt != nil {
		t.Fatalf("response-first trace: %+v", tr2)
	}
	if err := rec.RecordRequest(ctx, 8, "tr-2", "p", model.GuardVerdictPending); err != nil {
		t.Fatal(err)
	}
	tr2, _ = repo.FindByOrgTrace(ctx, 8, "tr-2")
	if tr2.Status != model.GuardTraceComplete || tr2.Verdict != model.GuardVerdictPass {
		t.Fatalf("after request: %+v", tr2)
	}

	// error 不覆盖已有裁决
	if err := rec.RecordResponse(ctx, 7, "tr-1", model.GuardVerdictError); err != nil {
		t.Fatal(err)
	}
	tr, _ = repo.FindByOrgTrace(ctx, 7, "tr-1")
	if tr.Verdict != model.GuardVerdictWouldBlock {
		t.Fatalf("error must not clobber verdict: %+v", tr)
	}
}
