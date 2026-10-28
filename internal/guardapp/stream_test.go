package guardapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/crosslink/internal/guardrail"
	"gorm.io/datatypes"
)

// newStreamTest 构建挂流式端点的测试 App：规则命中 "forbidden-token" 才
// 产生可断言的 would_block 事件；finalize 钩子直连管线递交。
func newStreamTest(t *testing.T, maxEntries int) (*gin.Engine, *App, string, *gorm.DB) {
	t.Helper()
	db, _, _, tokens := newTestApp(t)
	if err := db.Create(&guardrail.GuardrailRule{
		Name: "stream-forbidden", Type: "keyword_filter", Direction: "both", Enabled: true,
		Severity: "high", Action: "block",
		Config: datatypes.JSON([]byte(`{"blocklist":["forbidden-token"]}`)),
	}).Error; err != nil {
		t.Fatal(err)
	}
	tok, err := tokens.Create(t.Context(), 1, "stream-test")
	if err != nil {
		t.Fatal(err)
	}
	grSvc := guardrail.NewGuardrailService(db, nil)
	grSvc.RefreshSettings(t.Context())
	pool := worker.NewPool("guard-stream-test", 2, 8, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pool.Shutdown(ctx)
	})
	rec := NewTraceRecorder(repository.NewGuardTraceRepo(db), NewTraceStore(time.Minute, 100))
	cfg := &config.Config{}
	cfg.Guard.Ingest.MaxBodyBytes = 1 << 20
	a := &App{
		Cfg: cfg, DB: db, TokenRepo: repository.NewGuardTokenRepo(db), IngestCache: newTokenCache(),
		Tokens: tokens, Traces: rec, Pool: pool,
		Pipeline: NewPipeline(grSvc, repository.NewGuardEventRepo(db), rec, pool, 500*time.Millisecond),
		Idem:     newIdemStore(time.Minute, 1000),
		Limiter:  NewIngestRateLimiter(nil, 6000, 6000),
	}
	a.Streams = NewStreamStore(time.Minute, maxEntries, func(req *DetectRequest) { a.Pipeline.SubmitAsync(req) })
	gin.SetMode(gin.TestMode)
	r := gin.New()
	a.registerIngestRoutes(r)
	return r, a, tok.Token, db
}

func streamChunkBody(event string, seq int64, field, text string) string {
	f := ""
	if field != "" {
		f = fmt.Sprintf(`,"field":%q`, field)
	}
	return fmt.Sprintf(`{"event":%q,"seq":%d%s,"text":%q}`, event, seq, f, text)
}

// waitForEvent 轮询事件表直到出现目标 trace 的响应向命中事件。
func waitForStreamEvent(t *testing.T, db *gorm.DB, traceID string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		db.Table("guard_events").Where("trace_id = ? AND direction = ? AND action = ?",
			traceID, "response", "would_block").Count(&n)
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// 路径 1：分片累积 → final 终检 → 命中事件落库（分片拼接正确）。
func TestStreamChunkFinalizeE2E(t *testing.T) {
	r, _, tok, db := newStreamTest(t, 100)

	// 请求向先落（配对 prompt；此处仅验证请求向事件与终检共存）。
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect/async", tok,
		detectBody("stream-1", "hello")); w.Code != http.StatusAccepted {
		t.Fatalf("request leg: %d %s", w.Code, w.Body.String())
	}
	w := doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-1/stream", tok,
		streamChunkBody("chunk", 1, "content", "part one "))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"buffered":1`) {
		t.Fatalf("chunk 1: %d %s", w.Code, w.Body.String())
	}
	// 重复 seq 409（幂等）
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-1/stream", tok,
		streamChunkBody("chunk", 1, "content", "dup")); w.Code != http.StatusConflict {
		t.Fatalf("duplicate seq: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-1/stream", tok,
		streamChunkBody("chunk", 2, "content", "has forbidden-token in")); w.Code != http.StatusAccepted {
		t.Fatalf("chunk 2: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-1/stream", tok,
		streamChunkBody("chunk", 3, "reasoning", "internal thoughts")); w.Code != http.StatusAccepted {
		t.Fatalf("chunk 3: %d %s", w.Code, w.Body.String())
	}
	// final（自增 seq 不与分片撞幂等位）
	w = doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-1/stream", tok,
		streamChunkBody("final", 99, "", "part three"))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"finalized":true`) {
		t.Fatalf("final: %d %s", w.Code, w.Body.String())
	}
	if !waitForStreamEvent(t, db, "stream-1") {
		t.Fatal("finalized response event missing")
	}
	// 二次 final：缓冲已被取走 → no_buffer（且不同 seq 不撞幂等）
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-1/stream", tok,
		streamChunkBody("final", 100, "", "")); w.Code != http.StatusAccepted ||
		!strings.Contains(w.Body.String(), `"no_buffer"`) {
		t.Fatalf("second final: %d %s", w.Code, w.Body.String())
	}
	// final 后事件文本=content 拼接（reasoning 不混入）：预览断言
	var preview string
	db.Raw(`SELECT content_preview FROM guard_events WHERE trace_id = ? AND direction = 'response' AND action = 'would_block' LIMIT 1`, "stream-1").Scan(&preview)
	if !strings.Contains(preview, "part one has") || strings.Contains(preview, "internal thoughts") {
		t.Fatalf("preview shows wrong assembly: %q", preview)
	}
}

// 路径 1b：final 自身文本并入重组——关键词跨 chunk/final 边界必须命中；
// 且 final-only 流（无前置 chunk）也能一次性终检。
func TestStreamFinalTextIncluded(t *testing.T) {
	r, _, tok, db := newStreamTest(t, 100)

	// 跨界：chunk 携 "forbidden-"，final 携 "token tail"
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/split-1/stream", tok,
		streamChunkBody("chunk", 1, "content", "prefix forbidden-")); w.Code != http.StatusAccepted {
		t.Fatalf("chunk: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/split-1/stream", tok,
		streamChunkBody("final", 2, "content", "token tail")); w.Code != http.StatusAccepted ||
		!strings.Contains(w.Body.String(), `"finalized":true`) {
		t.Fatalf("final: %d %s", w.Code, w.Body.String())
	}
	if !waitForStreamEvent(t, db, "split-1") {
		t.Fatal("keyword split across chunk/final missed — final text dropped from reassembly")
	}

	// final-only：单次 final 直接携带全文
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/only-1/stream", tok,
		streamChunkBody("final", 1, "content", "solo forbidden-token shot")); w.Code != http.StatusAccepted ||
		!strings.Contains(w.Body.String(), `"finalized":true`) {
		t.Fatalf("final-only: %d %s", w.Code, w.Body.String())
	}
	if !waitForStreamEvent(t, db, "only-1") {
		t.Fatal("final-only stream should be detected once as whole text")
	}
}

// 路径 2：客户端断连（只 chunk 不 final）→ 空闲终检兜底。
func TestStreamIdleFinalize(t *testing.T) {
	r, a, tok, db := newStreamTest(t, 100)
	// 压缩空闲窗口（默认 5min 只适生产）
	a.Streams.idle = 60 * time.Millisecond

	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/stream-idle/stream", tok,
		streamChunkBody("chunk", 1, "", "idle forbidden-token end")); w.Code != http.StatusAccepted {
		t.Fatalf("chunk: %d %s", w.Code, w.Body.String())
	}
	if !waitForStreamEvent(t, db, "stream-idle") {
		t.Fatal("idle-finalized event missing")
	}
}

// 路径 3：容量淘汰——第二条流挤掉第一条（maxEntries=1），被淘汰流无终检。
func TestStreamCapacityEviction(t *testing.T) {
	r, a, tok, db := newStreamTest(t, 1)

	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/evict-a/stream", tok,
		streamChunkBody("chunk", 1, "", "a forbidden-token")); w.Code != http.StatusAccepted {
		t.Fatalf("chunk a: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/evict-b/stream", tok,
		streamChunkBody("chunk", 1, "", "b forbidden-token")); w.Code != http.StatusAccepted {
		t.Fatalf("chunk b: %d", w.Code)
	}
	// a 被淘汰：再 final a → no_buffer；b 仍可 final
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/evict-a/stream", tok,
		streamChunkBody("final", 9, "", "")); !strings.Contains(w.Body.String(), `"no_buffer"`) {
		t.Fatalf("evicted a final: %s", w.Body.String())
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/evict-b/stream", tok,
		streamChunkBody("final", 9, "", "")); !strings.Contains(w.Body.String(), `"finalized":true`) {
		t.Fatalf("kept b final: %s", w.Body.String())
	}
	if !waitForStreamEvent(t, db, "evict-b") {
		t.Fatal("b event missing")
	}
	if _, lru := a.Streams.Evicted(); lru != 1 {
		t.Fatalf("lru evictions = %d, want 1", lru)
	}
	// a 无终检事件（被淘汰不检测）；唯一事件是 no_buffer final 落的覆盖
	// 缺口（action=log）——丢弃也要留观测痕迹。
	time.Sleep(100 * time.Millisecond)
	var n int64
	db.Table("guard_events").Where("trace_id = ? AND action != ?", "evict-a", "log").Count(&n)
	if n != 0 {
		t.Fatalf("evicted stream produced %d detection events, want 0", n)
	}
	db.Table("guard_events").Where("trace_id = ? AND action = ?", "evict-a", "log").Count(&n)
	if n != 1 {
		t.Fatalf("gap events = %d, want 1", n)
	}
}

// 入口校验：事件类型/字段白名单、无 token 401。
func TestStreamValidation(t *testing.T) {
	r, _, tok, _ := newStreamTest(t, 100)

	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/v-1/stream", tok,
		`{"event":"weird","seq":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad event: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/v-1/stream", tok,
		streamChunkBody("chunk", 1, "tool_call", "x")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad field: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/v-1/stream", "",
		streamChunkBody("chunk", 1, "", "x")); w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
}
