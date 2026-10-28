package guardapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/worker"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// bootstrapPassword 与 guardapp_test.go newTestApp 的 seed 配置一致。
const bootstrapPassword = "not-default"

// newTestAdminAPI 组管理面测试服务：真实迁移链 + Bootstrap 种子 + 权限缓存。
// 登录节流是包级全局，测试间复位避免串扰。
func newTestAdminAPI(t *testing.T) (*gin.Engine, *App, *gorm.DB) {
	t.Helper()
	db, tokenRepo, cache, tokens := newTestApp(t)
	t.Cleanup(func() {
		logins.mu.Lock()
		logins.fails = make(map[string][]time.Time)
		logins.mu.Unlock()
	})
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Admin = config.AdminConfig{
		Username: "admin", JWTSecret: strings.Repeat("test-secret-", 4), TokenExpiry: 1,
	}
	grSvc := guardrail.NewGuardrailService(db, nil)
	grSvc.RefreshSettings(context.Background())
	pc := middleware.NewPermissionCache(repository.NewRoleRepo(db))
	if err := pc.Load(); err != nil {
		t.Fatal(err)
	}
	a := &App{
		Cfg: cfg, DB: db, CP: cp, Guardrail: grSvc, PermCache: pc,
		TokenRepo: tokenRepo, IngestCache: cache, Tokens: tokens,
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	a.registerAdminRoutes(r)
	return r, a, db
}

// newTestGuardService 在同一路由上同时挂管理面与接入面（端到端：管理面建
// 的规则要真实进入检测管线）。
func newTestAdminAPIWithIngest(t *testing.T) (*gin.Engine, *App, *gorm.DB) {
	t.Helper()
	r, a, db := newTestAdminAPI(t)
	pool := worker.NewPool("guard-admin-test", 2, 8, 0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pool.Shutdown(ctx)
	})
	a.Pool = pool
	a.Traces = NewTraceRecorder(repository.NewGuardTraceRepo(db), NewTraceStore(time.Minute, 100))
	a.Pipeline = NewPipeline(a.Guardrail, repository.NewGuardEventRepo(db), a.Traces, pool, 500*time.Millisecond)
	a.Idem = newIdemStore(time.Minute, 1000)
	a.Limiter = NewIngestRateLimiter(nil, 6000, 6000)
	a.registerIngestRoutes(r)
	return r, a, db
}

// loginAs 返回登录 token（不做强制改密——种子管理员首登即锁管理面）。
func loginAs(t *testing.T, r *gin.Engine, username, password string) string {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "",
		fmtJSON(username, password))
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", username, w.Code, w.Body.String())
	}
	return jsonStr(t, w.Body.Bytes(), "token")
}

// adminSession 登录 + 完成强制改密，返回解锁后的 token。
func adminSession(t *testing.T, r *gin.Engine) string {
	t.Helper()
	tok := loginAs(t, r, "admin", bootstrapPassword)
	w := doJSON(t, r, http.MethodPost, "/admin/api/auth/change-forced-password", tok,
		`{"current_password":"`+bootstrapPassword+`","new_password":"Unlocked-456","confirm_password":"Unlocked-456"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("change-forced-password: %d %s", w.Code, w.Body.String())
	}
	return jsonStr(t, w.Body.Bytes(), "token")
}

func fmtJSON(username, password string) string {
	return `{"username":"` + username + `","password":"` + password + `"}`
}

// jsonStr 从 {"data":{"<field>":...}} 或顶层 {"<field>":...} 取字符串字段。
func jsonStr(t *testing.T, body []byte, field string) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("bad json: %v (%s)", err, body)
	}
	raw := top[field]
	if raw == nil {
		if data := top["data"]; data != nil {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(data, &inner); err == nil {
				raw = inner[field]
			}
		}
	}
	if raw == nil {
		t.Fatalf("field %q missing in %s", field, body)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %q not a string: %s", field, body)
	}
	return s
}

// createTestUser 建指定列用户（sqlite 约束同 bootstrap：不整行 Create）。
// status=0 必须走 map 创建——Status 列带 default:1 标签，gorm 结构体创建
// 会略过零值列让 DB 默认值生效，Select 也压不住。
func createTestUser(t *testing.T, db *gorm.DB, username, roleName string, status int16) {
	t.Helper()
	roleID, err := roleIDByName(db, roleName)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("member-pass-123"), bcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.User{}).Create(map[string]any{
		"username": username, "password_hash": string(hash), "display_name": username,
		"role_id": roleID, "status": status,
		"force_password_change": false, "created_at": time.Now(), "updated_at": time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

// 路径 1：登录 → 强制改密闸门 → 改密解锁 → 旧密码失效
func TestAdminLoginAndForcePasswordChange(t *testing.T) {
	r, _, _ := newTestAdminAPI(t)

	tok := loginAs(t, r, "admin", bootstrapPassword)
	// 未改密：管理面 403（放行码 force_password_change），改密端点本身可达
	w := doJSON(t, r, http.MethodGet, "/admin/api/guardrails", tok, "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "force_password_change") {
		t.Fatalf("pre-change guardrails: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/admin/api/auth/permissions", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("permissions (allowlisted): %d %s", w.Code, w.Body.String())
	}
	// 当前密码错误 → 401，闸门保持
	w = doJSON(t, r, http.MethodPost, "/admin/api/auth/change-forced-password", tok,
		`{"current_password":"wrong","new_password":"NewPass-789","confirm_password":"NewPass-789"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current: %d %s", w.Code, w.Body.String())
	}

	unlocked := adminSession(t, r)
	w = doJSON(t, r, http.MethodGet, "/admin/api/guardrails", unlocked, "")
	if w.Code != http.StatusOK {
		t.Fatalf("post-change guardrails: %d %s", w.Code, w.Body.String())
	}
	// 旧密码失效、新密码可登录
	if w := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("admin", bootstrapPassword)); w.Code != http.StatusUnauthorized {
		t.Fatalf("old password should fail: %d", w.Code)
	}
	if w := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("admin", "Unlocked-456")); w.Code != http.StatusOK {
		t.Fatalf("new password login: %d %s", w.Code, w.Body.String())
	}
}

// 路径 2：失败登录面——密码错/用户不存在同响应、禁用账户、节流锁定
func TestAdminLoginFailures(t *testing.T) {
	r, _, db := newTestAdminAPI(t)

	if w := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("admin", "bad")); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body.String())
	}
	// 用户不存在：同 invalid_credentials 响应（不泄露存在性）
	w1 := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("ghost", "bad"))
	if w1.Code != http.StatusUnauthorized || strings.Contains(w1.Body.String(), "not found") {
		t.Fatalf("unknown user: %d %s", w1.Code, w1.Body.String())
	}
	createTestUser(t, db, "disabled-user", model.RoleMember, 0)
	if w := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("disabled-user", "member-pass-123")); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled: %d %s", w.Code, w.Body.String())
	}

	// 节流：admin 已 1 次失败，再 4 次打满 5 → 第 6 次（密码正确）也 429
	for i := 0; i < 4; i++ {
		doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("admin", "bad"))
	}
	if w := doJSON(t, r, http.MethodPost, "/admin/api/auth/login", "", fmtJSON("admin", bootstrapPassword)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("lockout: %d %s", w.Code, w.Body.String())
	}
}

// 路径 3：RBAC——member 角色无动作位 403；被授予 guard:event_view 后卡在
// org 上下文缺失（requireOrg），证明两道闸各司其职。
func TestAdminRBAC(t *testing.T) {
	r, a, db := newTestAdminAPI(t)
	// 给 member 角色授 guard:event_view（在缓存装载前写入——缓存反映装载时点）
	memberRoleID, err := roleIDByName(db, model.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.RolePermission{RoleID: memberRoleID, Action: "guard:event_view"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.PermCache.Load(); err != nil {
		t.Fatal(err)
	}
	createTestUser(t, db, "member1", model.RoleMember, 1)

	tok := loginAs(t, r, "member1", "member-pass-123")
	for _, tc := range []struct{ path string }{
		{"/admin/api/guardrails"},
		{"/admin/api/guard/tokens"},
	} {
		if w := doJSON(t, r, http.MethodGet, tc.path, tok, ""); w.Code != http.StatusForbidden {
			t.Fatalf("member %s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	// 有动作位但无 org 上下文：403 no_org_context
	if w := doJSON(t, r, http.MethodGet, "/admin/api/guard/events", tok, ""); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "no_org_context") {
		t.Fatalf("member events: %d %s", w.Code, w.Body.String())
	}
	// 权限端点反映角色动作位
	w := doJSON(t, r, http.MethodGet, "/admin/api/auth/permissions", tok, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "guard:event_view") {
		t.Fatalf("member permissions: %d %s", w.Code, w.Body.String())
	}
}

// 路径 4：规则 CRUD（复用网关 handler）+ 管理面建的规则真实进入检测管线
func TestAdminGuardrailCRUDAndDetection(t *testing.T) {
	r, a, db := newTestAdminAPIWithIngest(t)
	tok := adminSession(t, r)

	// 端到端准备：ingest 凭证（default org = 种子 org id 1）
	ingestTok, err := a.Tokens.Create(context.Background(), 1, "admin-crud-test")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"name":"admin-rule","type":"keyword_filter","direction":"both","config":{"blocklist":["admin-secret-42"]}}`
	w := doJSON(t, r, http.MethodPost, "/admin/api/guardrails", tok, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID    int64  `json:"id"`
			OrgID *int64 `json:"org_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == 0 {
		t.Fatalf("create body: %s", w.Body.String())
	}
	if created.Data.OrgID == nil || *created.Data.OrgID != 1 {
		t.Fatalf("rule must be org-scoped to admin org 1, got %v", created.Data.OrgID)
	}
	id := created.Data.ID

	// 管理面建的规则即刻生效：同 org 检测命中 block
	w = doJSON(t, r, http.MethodPost, "/api/v1/detect", ingestTok.Token,
		`{"trace_id":"admin-crud-1","protocol":"generic","content":"leak admin-secret-42 here"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"verdict":"block"`) {
		t.Fatalf("detect with admin-created rule: %d %s", w.Code, w.Body.String())
	}

	// test 端点（规则引擎单测）
	w = doJSON(t, r, http.MethodPost, "/admin/api/guardrails/"+itoa(id)+"/test", tok, `{"text":"x admin-secret-42"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rule test: %d %s", w.Code, w.Body.String())
	}
	// 更新 → 禁用后不再命中
	if w := doJSON(t, r, http.MethodPut, "/admin/api/guardrails/"+itoa(id), tok, `{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("update rule: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/api/v1/detect", ingestTok.Token,
		`{"trace_id":"admin-crud-2","protocol":"generic","content":"leak admin-secret-42 again"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"verdict":"allow"`) {
		t.Fatalf("disabled rule must not hit: %d %s", w.Code, w.Body.String())
	}
	// 删除
	if w := doJSON(t, r, http.MethodDelete, "/admin/api/guardrails/"+itoa(id), tok, ""); w.Code != http.StatusOK {
		t.Fatalf("delete rule: %d %s", w.Code, w.Body.String())
	}
	var n int64
	db.Model(&guardrail.GuardrailRule{}).Where("id = ?", id).Count(&n)
	if n != 0 {
		t.Fatalf("rule row must be gone")
	}
}

func itoa(v int64) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(string(rune(0))+fmtItoa(v), string(rune(0)), ""), " ", ""))
}

func fmtItoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// 路径 5：ingest 凭证 CRUD——明文只出现一次、列表不回显哈希、吊销/重置
func TestAdminTokens(t *testing.T) {
	r, _, _ := newTestAdminAPI(t)
	tok := adminSession(t, r)

	w := doJSON(t, r, http.MethodPost, "/admin/api/guard/tokens", tok, `{"name":"edge-mirror"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create token: %d %s", w.Code, w.Body.String())
	}
	plain1 := jsonStr(t, w.Body.Bytes(), "token")
	if !strings.HasPrefix(plain1, "cg-") {
		t.Fatalf("token prefix: %s", plain1)
	}

	w = doJSON(t, r, http.MethodGet, "/admin/api/guard/tokens", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list tokens: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token_hash") || strings.Contains(w.Body.String(), plain1) {
		t.Fatalf("token list must not leak hash/plaintext: %s", w.Body.String())
	}
	var list struct {
		Data []tokenView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data) != 1 {
		t.Fatalf("token list: %s", w.Body.String())
	}
	id := list.Data[0].ID
	if list.Data[0].Status != 1 || list.Data[0].KeyPrefix == "" {
		t.Fatalf("token view: %+v", list.Data[0])
	}

	if w := doJSON(t, r, http.MethodPost, "/admin/api/guard/tokens/"+fmtItoa(id)+"/revoke", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, r, http.MethodGet, "/admin/api/guard/tokens", tok, "")
	if !strings.Contains(w.Body.String(), `"status":0`) {
		t.Fatalf("revoked status: %s", w.Body.String())
	}
	w = doJSON(t, r, http.MethodPost, "/admin/api/guard/tokens/"+fmtItoa(id)+"/reset", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}
	if plain2 := jsonStr(t, w.Body.Bytes(), "token"); plain2 == plain1 || !strings.HasPrefix(plain2, "cg-") {
		t.Fatalf("reset must issue a new secret")
	}
}

// 路径 6：事件查询 org 强制隔离 + trace 过滤 + 统计只算本 org
func TestAdminEventsOrgFilterAndStats(t *testing.T) {
	r, _, db := newTestAdminAPI(t)
	tok := adminSession(t, r)

	events := repository.NewGuardEventRepo(db)
	traces := repository.NewGuardTraceRepo(db)
	ctx := context.Background()
	for _, e := range []model.GuardEvent{
		{TraceID: "t1", OrgID: 1, Direction: "request", Action: "would_block", CreatedAt: time.Now()},
		{TraceID: "t1", OrgID: 1, Direction: "response", Action: "log", CreatedAt: time.Now()},
		{TraceID: "t9", OrgID: 2, Direction: "request", Action: "would_block", CreatedAt: time.Now()},
	} {
		if err := events.Create(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	for _, tr := range []model.GuardTrace{
		{TraceID: "t1", OrgID: 1, Status: model.GuardTraceComplete, Verdict: model.GuardVerdictWouldBlock},
		{TraceID: "t2", OrgID: 1, Status: model.GuardTraceOpen, Verdict: model.GuardVerdictPending},
		{TraceID: "t9", OrgID: 2, Status: model.GuardTraceOpen, Verdict: model.GuardVerdictPending},
	} {
		if err := traces.Create(ctx, &tr); err != nil {
			t.Fatal(err)
		}
	}

	w := doJSON(t, r, http.MethodGet, "/admin/api/guard/events", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Data []model.GuardEvent `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("org filter must exclude org 2: %s", w.Body.String())
	}
	for _, e := range list.Data {
		if e.OrgID != 1 {
			t.Fatalf("leaked org: %+v", e)
		}
	}

	w = doJSON(t, r, http.MethodGet, "/admin/api/guard/events?trace_id=t1", tok, "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data) != 2 {
		t.Fatalf("trace filter: %s", w.Body.String())
	}
	for _, e := range list.Data {
		if e.TraceID != "t1" {
			t.Fatalf("trace filter leak: %+v", e)
		}
	}

	w = doJSON(t, r, http.MethodGet, "/admin/api/guard/stats", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("stats: %d %s", w.Code, w.Body.String())
	}
	s := w.Body.String()
	// org1 事件：would_block 1 + log 1；org2 的 would_block 不得计入
	if !strings.Contains(s, `"action":"would_block","count":1`) || !strings.Contains(s, `"action":"log","count":1`) {
		t.Fatalf("stats by_action: %s", s)
	}
	if !strings.Contains(s, `"verdict":"would_block","count":1`) {
		t.Fatalf("stats by_verdict: %s", s)
	}
	// open：org1 有 1（t2），org2 的 open 不得计入
	if !strings.Contains(s, `"open":1`) {
		t.Fatalf("stats open: %s", s)
	}
}
