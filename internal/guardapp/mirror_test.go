package guardapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// newMirrorAPI 组一个带镜像路由的测试服务：与 newTestAPI 同构（真实迁移
// 链库 + block 规则 + 管线），bodyLimit 可调小以测 413。
func newMirrorAPI(t *testing.T, bodyLimit int64) (*gin.Engine, *App, string, *gorm.DB) {
	t.Helper()
	db, _, _, tokens := newTestApp(t)
	if err := db.Create(&guardrail.GuardrailRule{
		Name: "mirror-forbidden", Type: "keyword_filter", Direction: "both", Enabled: true,
		Severity: "high", Action: "block",
		Config: datatypes.JSON([]byte(`{"blocklist":["forbidden-token"]}`)),
	}).Error; err != nil {
		t.Fatal(err)
	}
	tok, err := tokens.Create(context.Background(), 1, "mirror-test")
	if err != nil {
		t.Fatal(err)
	}
	grSvc := guardrail.NewGuardrailService(db, nil)
	grSvc.RefreshSettings(context.Background())
	pool := worker.NewPool("guard-mirror-test", 2, 8, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pool.Shutdown(ctx)
	})
	rec := NewTraceRecorder(repository.NewGuardTraceRepo(db), NewTraceStore(time.Minute, 100))
	cfg := &config.Config{}
	cfg.Guard.Ingest.MaxBodyBytes = bodyLimit
	a := &App{
		Cfg: cfg, DB: db, TokenRepo: repository.NewGuardTokenRepo(db), IngestCache: newTokenCache(),
		Tokens: tokens, Traces: rec, Pool: pool,
		Pipeline: NewPipeline(grSvc, repository.NewGuardEventRepo(db), rec, pool, 500*time.Millisecond),
		Idem:     newIdemStore(time.Minute, 1000),
		Limiter:  NewIngestRateLimiter(nil, 6000, 6000),
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	a.registerIngestRoutes(r)
	a.registerMirrorRoutes(r)
	return r, a, tok.Token, db
}

func doMirror(t *testing.T, r *gin.Engine, method, path, token string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// countEvents 轮询等镜像异步事件落库（方向/动作过滤）。
func countEvents(t *testing.T, db *gorm.DB, dir, action string) int64 {
	t.Helper()
	var n int64
	waitFor(t, 3*time.Second, func() bool {
		q := db.Table("guard_events").Where("direction = ? AND action = ?", dir, action)
		if action == "" {
			q = db.Table("guard_events").Where("direction = ?", dir)
		}
		q.Count(&n)
		return n > 0
	}, "mirror events not persisted")
	return n
}

// 请求向镜像：openai 报文命中 → 生成 trace + X-Guard-Trace 回带 + 事件落库；
// 自报 x-request-id 沿用且重复回放 duplicate（不重复落事件）。
func TestMirrorRequestOpenAI(t *testing.T) {
	r, a, tok, db := newMirrorAPI(t, 0)

	hit := []byte(`{"model":"gpt-x","messages":[{"role":"user","content":"leak forbidden-token here"}]}`)
	w := doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", tok, hit, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("accepted: %d %s", w.Code, w.Body.String())
	}
	trace := w.Header().Get("X-Guard-Trace")
	if trace == "" || !strings.HasPrefix(trace, mirrorTracePrefix) {
		t.Fatalf("expected generated trace header, got %q", trace)
	}
	if n := countEvents(t, db, "request", "would_block"); n != 1 {
		t.Fatalf("want 1 would_block event, got %d", n)
	}

	// 自报 trace：沿用 + 重复回放 duplicate:true，事件不翻倍
	hdr := map[string]string{"X-Request-Id": "cust-abc-123"}
	w = doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", tok, hit, hdr)
	if w.Code != http.StatusAccepted || w.Header().Get("X-Guard-Trace") != "cust-abc-123" {
		t.Fatalf("echo trace: %d %s", w.Code, w.Header().Get("X-Guard-Trace"))
	}
	w = doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", tok, hit, hdr)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate replay: %d %s", w.Code, w.Body.String())
	}
	time.Sleep(200 * time.Millisecond) // 给异步管线静默窗口
	var n int64
	db.Table("guard_events").Where("action = ?", "would_block").Count(&n)
	if n != 2 {
		t.Fatalf("duplicate replay must not re-run pipeline, want 2 events got %d", n)
	}
	_ = a
}

// anthropic 端点报文抽取 + gzip 解压路径。
func TestMirrorRequestAnthropicAndGzip(t *testing.T) {
	r, _, tok, db := newMirrorAPI(t, 0)

	body := []byte(`{"model":"claude-x","system":"be safe","messages":[{"role":"user","content":[{"type":"text","text":"also forbidden-token"}]}]}`)
	w := doMirror(t, r, http.MethodPost, "/mirror/v1/messages", tok, body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("accepted: %d %s", w.Code, w.Body.String())
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(body)
	zw.Close()
	w = doMirror(t, r, http.MethodPost, "/mirror/v1/messages", tok, buf.Bytes(), map[string]string{"Content-Encoding": "gzip"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("gzip accepted: %d %s", w.Code, w.Body.String())
	}
	if n := countEvents(t, db, "request", "would_block"); n != 2 {
		t.Fatalf("want 2 would_block events (raw+gzip), got %d", n)
	}
}

// 413/415 拒绝路径：拒绝计事件（覆盖缺口），回明确错误码。
func TestMirrorRejects(t *testing.T) {
	r, _, tok, db := newMirrorAPI(t, 64)

	w := doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", tok,
		[]byte(`{"messages":[{"role":"user","content":"`+strings.Repeat("x", 200)+`"}]}`), nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("413: %d", w.Code)
	}

	w = doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", tok,
		[]byte(`{}`), map[string]string{"Content-Encoding": "br"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("415: %d", w.Code)
	}
	if n := countEvents(t, db, "request", "log"); n < 1 {
		t.Fatalf("reject event not recorded")
	}
}

// 响应向镜像：openai choices 形态、anthropic content blocks 形态、
// openai SSE 分片重组，命中各落一条 response 向事件。
func TestMirrorResponseShapes(t *testing.T) {
	r, _, tok, db := newMirrorAPI(t, 0)

	// 先镜像请求向（配对窗口入 prompt）
	reqBody := []byte(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	w := doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", tok, reqBody, map[string]string{"X-Request-Id": "mr-resp"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("request replay: %d", w.Code)
	}

	// openai 非流式响应形态
	w = doMirror(t, r, http.MethodPost, "/mirror/traces/mr-resp/response", tok,
		[]byte(`{"model":"gpt-x","choices":[{"message":{"role":"assistant","content":"answer has forbidden-token"}}]}`), nil)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("openai response: %d %s", w.Code, w.Body.String())
	}

	// anthropic 非流式响应形态（顶层 content blocks）
	w = doMirror(t, r, http.MethodPost, "/mirror/traces/mr-resp2/response", tok,
		[]byte(`{"model":"claude-x","content":[{"type":"text","text":"claude says forbidden-token"}]}`), nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("anthropic response: %d %s", w.Code, w.Body.String())
	}

	// openai SSE 全量捕获（终检重组跨分片命中）
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"stream says for"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"bidden-token split"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	req := httptest.NewRequest(http.MethodPost, "/mirror/traces/mr-resp3/response", strings.NewReader(sse))
	req.Header.Set("Content-Type", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("sse response: %d %s", rec.Code, rec.Body.String())
	}

	// anthropic SSE 形态（content_block_delta）
	sse2 := strings.Join([]string{
		`data: {"type":"message_start"}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"anthropic stream forbidden-token"}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	req2 := httptest.NewRequest(http.MethodPost, "/mirror/traces/mr-resp4/response", strings.NewReader(sse2))
	req2.Header.Set("Content-Type", "text/event-stream")
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("anthropic sse: %d %s", rec2.Code, rec2.Body.String())
	}

	if n := countEvents(t, db, "response", "would_block"); n < 4 {
		t.Fatalf("want >=4 response would_block events, got %d", n)
	}
	// trace 生命周期闭合（请求向 + 至少一个响应向完成）
	waitFor(t, 3*time.Second, func() bool {
		var status string
		if err := db.Raw(`SELECT status FROM guard_traces WHERE trace_id = ?`, "mr-resp").Scan(&status).Error; err != nil {
			return false
		}
		return status == "complete"
	}, "trace not closed after response replay")
}

// 幂等：响应向重复回放 duplicate:true。
func TestMirrorResponseDuplicate(t *testing.T) {
	r, _, tok, _ := newMirrorAPI(t, 0)
	body := []byte(`{"choices":[{"message":{"content":"clean"}}]}`)
	w := doMirror(t, r, http.MethodPost, "/mirror/traces/mr-dup/response", tok, body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first: %d", w.Code)
	}
	w = doMirror(t, r, http.MethodPost, "/mirror/traces/mr-dup/response", tok, body, nil)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
}

// 认证与限速与结构化 API 同链：无凭证 401；原始路径别名与 /mirror/* 同行为。
func TestMirrorAuth(t *testing.T) {
	r, _, tok, db := newMirrorAPI(t, 0)
	w := doMirror(t, r, http.MethodPost, "/mirror/v1/chat/completions", "", []byte(`{}`), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized: %d", w.Code)
	}
	// Envoy 纯配置档别名：主路径接收，行为一致
	w = doMirror(t, r, http.MethodPost, "/v1/chat/completions", tok,
		[]byte(`{"messages":[{"role":"user","content":"alias forbidden-token"}]}`), nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("alias: %d %s", w.Code, w.Body.String())
	}
	if n := countEvents(t, db, "request", "would_block"); n != 1 {
		t.Fatalf("alias events: %d", n)
	}
}

// reassembleSSE/mirrorResponseText 纯函数面：畸形行容忍、parts 数组、
// tool_result 内嵌文本、无文本返回空。
func TestMirrorParsers(t *testing.T) {
	if got := reassembleSSE([]byte("data: not-json\ndata: [DONE]\n")); got != "" {
		t.Fatalf("malformed lines must be skipped, got %q", got)
	}
	if got := reassembleSSE([]byte("event: x\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n")); got != "a" {
		t.Fatalf("CRLF lines, got %q", got)
	}
	s, u, ok := mirrorResponseText([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"image_url"}]}}]}`))
	if !ok || s != "a" || !u {
		t.Fatalf("parts+media: %q uninspectable=%v ok=%v", s, u, ok)
	}
	s, u, ok = mirrorResponseText([]byte(`{"content":[{"type":"tool_result","content":"tool output"}]}`))
	if !ok || s != "tool output" || u {
		t.Fatalf("tool_result: %q %v %v", s, u, ok)
	}
	if _, _, ok := mirrorResponseText([]byte(`{"unrelated":1}`)); ok {
		t.Fatal("unrelated shape must not match")
	}
	if got := mirrorModel([]byte(`{"model":"m1"}`)); got != "m1" {
		t.Fatalf("model: %q", got)
	}
	// trace 白名单：合法 ID 沿用（x-request-id 优先于 x-guard-trace），
	// 非法字符/超长回退生成型。
	ginCtx := func(req *http.Request) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req
		return c
	}
	req := httptest.NewRequest(http.MethodPost, "/mirror", nil)
	req.Header.Set("X-Request-Id", "abc_123.v4:5")
	if got := mirrorTraceID(ginCtx(req)); got != "abc_123.v4:5" {
		t.Fatalf("whitelisted id not honored: %q", got)
	}
	req.Header.Set("X-Request-Id", "bad id with spaces")
	if got := mirrorTraceID(ginCtx(req)); !strings.HasPrefix(got, mirrorTracePrefix) {
		t.Fatalf("illegal chars must fall back to generated, got %q", got)
	}
	req.Header.Set("X-Request-Id", strings.Repeat("a", 65))
	if got := mirrorTraceID(ginCtx(req)); !strings.HasPrefix(got, mirrorTracePrefix) {
		t.Fatalf("overlong id must fall back to generated, got %q", got)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/mirror", nil)
	if got := mirrorTraceID(ginCtx(req2)); !strings.HasPrefix(got, mirrorTracePrefix) {
		t.Fatalf("missing id must generate, got %q", got)
	}
}
