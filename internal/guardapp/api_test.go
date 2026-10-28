package guardapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// newTestAPI 组一个带 ingest 路由的测试服务：真实迁移链库 + block 规则 +
// 管线/幂等/限速（无 Redis → 限速走进程内降级窗口，正好覆盖该路径）。
// bodyLimit 字节上限可调小以测 413；tokenRPM/orgRPM 可调小以测 429。
func newTestAPI(t *testing.T, bodyLimit int64, tokenRPM, orgRPM int) (*gin.Engine, *App, string, *gorm.DB) {
	t.Helper()
	db, _, _, tokens := newTestApp(t)
	if err := db.Create(&guardrail.GuardrailRule{
		Name: "api-forbidden", Type: "keyword_filter", Direction: "both", Enabled: true,
		Severity: "high", Action: "block",
		Config: datatypes.JSON([]byte(`{"blocklist":["forbidden-token"]}`)),
	}).Error; err != nil {
		t.Fatal(err)
	}
	tok, err := tokens.Create(context.Background(), 1, "api-test")
	if err != nil {
		t.Fatal(err)
	}
	grSvc := guardrail.NewGuardrailService(db, nil)
	grSvc.RefreshSettings(context.Background())
	pool := worker.NewPool("guard-api-test", 2, 8, 0)
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
		Limiter:  NewIngestRateLimiter(nil, tokenRPM, orgRPM),
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	a.registerIngestRoutes(r)
	return r, a, tok.Token, db
}

func doJSON(t *testing.T, r *gin.Engine, method, path, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func detectBody(traceID, content string) string {
	return fmt.Sprintf(`{"trace_id":%q,"protocol":"generic","model":"m","content":%q}`, traceID, content)
}

// 路径 1：成功——同步裁决 block + 事件/trace 落库；响应向配对 complete
func TestAPIDetectSuccessAndPairing(t *testing.T) {
	r, _, tok, db := newTestAPI(t, 1<<20, 6000, 6000)

	w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, detectBody("api-1", "hello forbidden-token here"))
	if w.Code != http.StatusOK {
		t.Fatalf("detect: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"verdict":"block"`) ||
		!strings.Contains(w.Body.String(), `"action":"would_block"`) {
		t.Fatalf("verdict body: %s", w.Body.String())
	}
	// 无 token 401
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", "", detectBody("x", "c")); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth: %d", w.Code)
	}

	// 响应向配对：干净响应 → trace complete，verdict 汇总保持 would_block
	w = doJSON(t, r, http.MethodPost, "/api/v1/traces/api-1/response", tok,
		`{"protocol":"generic","content":"clean answer"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, 2*time.Second, func() bool {
		var n int64
		db.Model(&model.GuardEvent{}).Where("org_id = ? AND trace_id = ?", int64(1), "api-1").Count(&n)
		if n != 1 {
			return false
		}
		tr, err := repository.NewGuardTraceRepo(db).FindByOrgTrace(context.Background(), 1, "api-1")
		return err == nil && tr.Status == model.GuardTraceComplete
	}, "paired trace must complete with single event")
}

// 路径 2：超限 413
func TestAPIBodyTooLarge(t *testing.T) {
	r, _, tok, _ := newTestAPI(t, 64, 6000, 6000)
	big := detectBody("api-big", strings.Repeat("x", 200))
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d %s", w.Code, w.Body.String())
	}
}

// 路径 3：租户错配 403（数字串与名称两种匹配面）
func TestAPITenantMismatch(t *testing.T) {
	r, _, tok, _ := newTestAPI(t, 1<<20, 6000, 6000)
	body := `{"trace_id":"api-t","protocol":"generic","tenant":"999","content":"hi"}`
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, body); w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", w.Code, w.Body.String())
	}
	// org ID 数字串匹配放行；org 名称（default org）匹配放行
	for _, tenant := range []string{"1", "default"} {
		body := fmt.Sprintf(`{"trace_id":"api-t-%s","protocol":"generic","tenant":%q,"content":"hi"}`, tenant, tenant)
		if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, body); w.Code != http.StatusOK {
			t.Fatalf("tenant %q: %d %s", tenant, w.Code, w.Body.String())
		}
	}
}

// 路径 4：幂等——同 (trace, direction, seq) 二次提交 409，事件不重复
func TestAPIIdempotency(t *testing.T) {
	r, _, tok, db := newTestAPI(t, 1<<20, 6000, 6000)
	body := detectBody("api-dup", "forbidden-token twice")
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, body); w.Code != http.StatusOK {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, body); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	// 不同 seq 是新提交
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok,
		fmt.Sprintf(`{"trace_id":"api-dup","protocol":"generic","seq":2,"content":"forbidden-token"}`)); w.Code != http.StatusOK {
		t.Fatalf("seq=2: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, 2*time.Second, func() bool {
		var n int64
		db.Model(&model.GuardEvent{}).Where("trace_id = ?", "api-dup").Count(&n)
		return n == 2
	}, "two distinct submissions must persist two events")
}

// 路径 5：两级限速 429（无 Redis → 进程内降级全局窗口，上限 org_rpm）
func TestAPIRateLimit(t *testing.T) {
	r, _, tok, _ := newTestAPI(t, 1<<20, 6000, 2)
	for i := 0; i < 2; i++ {
		if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok,
			detectBody(fmt.Sprintf("api-rl-%d", i), "clean")); w.Code != http.StatusOK {
			t.Fatalf("req %d should pass: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok, detectBody("api-rl-x", "clean"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After: %q", w.Header().Get("Retry-After"))
	}
}

// 异步 202 + 队列饱和 202/dropped 已在 pipeline 测试覆盖递交面；此处断言
// 正常异步提交的事件落库与响应结构
func TestAPIAsyncAccepted(t *testing.T) {
	r, _, tok, db := newTestAPI(t, 1<<20, 6000, 6000)
	w := doJSON(t, r, http.MethodPost, "/api/v1/detect/async", tok, detectBody("api-async", "forbidden-token"))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"status":"accepted"`) {
		t.Fatalf("async: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, 2*time.Second, func() bool {
		var n int64
		db.Model(&model.GuardEvent{}).Where("trace_id = ?", "api-async").Count(&n)
		return n == 1
	}, "async event must persist")
}

// 协议白名单与无可检文本 400
func TestAPIValidation(t *testing.T) {
	r, _, tok, _ := newTestAPI(t, 1<<20, 6000, 6000)
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok,
		`{"trace_id":"v1","protocol":"grpc","content":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("protocol: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok,
		`{"trace_id":"v2","protocol":"generic","content":123}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no-text: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/api/v1/detect", tok,
		`{"trace_id":"","protocol":"generic","content":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty trace: %d", w.Code)
	}
	// path/payload trace 不一致
	if w := doJSON(t, r, http.MethodPost, "/api/v1/traces/aaa/response", tok,
		`{"trace_id":"bbb","protocol":"generic","content":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("mismatch: %d", w.Code)
	}
}
