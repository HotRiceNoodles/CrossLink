package guardapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newTestApp 建内存库 + 应用真实 sqlite 迁移链（AutoMigrate 会把时间列建
// 成 DATETIME，掩盖共享表 TEXT 时间列的扫描问题——2026-10-27 真机启动正
// 是栽在这上面，故测试必须跑真实 schema）+ 完成 bootstrap。
func newTestApp(t *testing.T) (*gorm.DB, *repository.GuardTokenRepo, *tokenCache, *IngestTokenService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	// :memory: 每个连接是独立数据库：worker 池并发写入会打开第二连接、
	// 落到没有迁移的空库（"no such table"）。测试强制单连接。
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	migrateSQLiteChain(t, db)
	cfg := &config.AdminConfig{Username: "admin", Password: "not-default"}
	if err := Bootstrap(db, cfg); err != nil {
		t.Fatal(err)
	}
	repo := repository.NewGuardTokenRepo(db)
	cache := newTokenCache()
	return db, repo, cache, NewIngestTokenService(repo, cache)
}

// migrateSQLiteChain 按序执行 migrations/sqlite/*.up.sql（与 dialect 包迁
// 移测试同一套拆分规则：无触发器/存储过程，按 ";\n" 切分安全）。
func migrateSQLiteChain(t *testing.T, db *gorm.DB) {
	t.Helper()
	entries, err := os.ReadDir("../../migrations/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile("../../migrations/sqlite/" + f)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range splitSQL(string(raw)) {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatalf("%s: %v\nstatement: %.200s", f, err, stmt)
			}
		}
	}
}

func splitSQL(sqlText string) []string {
	normalized := strings.ReplaceAll(sqlText, "\r\n", "\n")
	var stmts []string
	for _, chunk := range strings.Split(normalized, ";\n") {
		body := strings.TrimSpace(chunk)
		if body == "" {
			continue
		}
		onlyComments := true
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
				onlyComments = false
				break
			}
		}
		if !onlyComments {
			stmts = append(stmts, body)
		}
	}
	return stmts
}

func TestBootstrapSeedsIdempotently(t *testing.T) {
	db, _, _, _ := newTestApp(t)

	// 系统角色 + admin 用户 + default org
	var roleCount, userCount, orgCount int64
	db.Model(&model.Role{}).Where("is_system = ?", true).Count(&roleCount)
	db.Model(&model.User{}).Count(&userCount)
	db.Model(&model.Organization{}).Count(&orgCount)
	if roleCount != 3 || userCount != 1 || orgCount != 1 {
		t.Fatalf("seed counts: roles=%d users=%d orgs=%d", roleCount, userCount, orgCount)
	}

	// admin 权限齐备（ValidActions 全量）
	adminRoleID, err := roleIDByName(db, model.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	var permCount int64
	db.Model(&model.RolePermission{}).Where("role_id = ?", adminRoleID).Count(&permCount)
	if permCount != int64(len(model.ValidActions)) {
		t.Fatalf("admin permissions: got %d want %d", permCount, len(model.ValidActions))
	}

	// 幂等：重复 Bootstrap 不再创建
	if err := Bootstrap(db, &config.AdminConfig{Username: "admin", Password: "not-default"}); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	db.Model(&model.User{}).Count(&userCount)
	if userCount != 1 {
		t.Fatalf("bootstrap not idempotent: users=%d", userCount)
	}

	// 默认密码拒绝启动
	if err := Bootstrap(db, &config.AdminConfig{Username: "admin", Password: "changeme"}); err == nil {
		t.Fatal("default password must be rejected")
	}
}

func TestIngestTokenLifecycle(t *testing.T) {
	db, repo, _, svc := newTestApp(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, 1, "edge-mirror")
	if err != nil {
		t.Fatal(err)
	}
	// 库中无明文，只有哈希
	row, err := repo.FindByHash(ctx, HashToken(created.Token))
	if err != nil {
		t.Fatal(err)
	}
	if row.OrgID != 1 || row.Name != "edge-mirror" || row.Status != 1 {
		t.Fatalf("stored token: %+v", row)
	}
	var plaintextCount int64
	db.Model(&model.GuardIngestToken{}).Where("token_hash = ?", created.Token).Count(&plaintextCount)
	if plaintextCount != 0 {
		t.Fatal("plaintext token must never be stored")
	}

	// Reset：行 ID 不变，旧明文作废，新明文可用
	reset, err := svc.Reset(ctx, 1, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.ID != created.ID {
		t.Fatalf("reset changed row id: %d -> %d", created.ID, reset.ID)
	}
	if _, err := repo.FindByHash(ctx, HashToken(created.Token)); err == nil {
		t.Fatal("old credential must be invalidated after reset")
	}
	if _, err := repo.FindByHash(ctx, HashToken(reset.Token)); err != nil {
		t.Fatalf("new credential not found: %v", err)
	}
}

func TestIngestAuthMiddleware(t *testing.T) {
	db, repo, cache, svc := newTestApp(t)
	ctx := context.Background()

	tokA, _ := svc.Create(ctx, 1, "org-a")
	revoked, _ := svc.Create(ctx, 1, "revoked")
	if err := svc.Revoke(ctx, 1, revoked.ID); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.POST("/ingest", IngestAuth(repo, cache), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"org_id": c.GetInt64("org_id"), "token_id": c.GetInt64("guard_token_id")})
	})

	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := do(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", w.Code)
	}
	if w := do("Bearer cg-does-not-exist"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", w.Code)
	}
	if w := do("Bearer " + revoked.Token); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token must 401 (single-node invalidate): %d", w.Code)
	}
	w := do("Bearer " + tokA.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("valid token: %d body=%s", w.Code, w.Body.String())
	}

	// 缓存命中路径上的吊销：invalidate 清缓存后立即 401
	if err := svc.Revoke(ctx, 1, tokA.ID); err != nil {
		t.Fatal(err)
	}
	if w := do("Bearer " + tokA.Token); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked after cache hit: %d", w.Code)
	}

	// TTL 过期后吊销生效（多节点一致性上界）：缓存命中后直改库绕过
	// invalidate，再前进时钟越过 TTL，模拟未收到失效通知的节点。
	tokB, _ := svc.Create(ctx, 1, "ttl")
	if w := do("Bearer " + tokB.Token); w.Code != http.StatusOK {
		t.Fatalf("pre-revoke: %d", w.Code)
	}
	base := time.Now()
	cache.now = func() time.Time { return base.Add(cache.ttl + time.Second) }
	db.Model(&model.GuardIngestToken{}).Where("id = ?", tokB.ID).
		Updates(map[string]any{"status": 0})
	if w := do("Bearer " + tokB.Token); w.Code != http.StatusUnauthorized {
		t.Fatalf("stale cache must not outlive TTL: %d", w.Code)
	}
}

func TestRequirePayloadOrg(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/ingest", func(c *gin.Context) {
		c.Set("org_id", int64(7))
		payloadOrg, _ := strconv.ParseInt(c.Query("org"), 10, 64)
		if !RequirePayloadOrg(c, payloadOrg) {
			return
		}
		c.Status(http.StatusOK)
	})

	cases := []struct {
		payloadOrg int64
		want       int
	}{
		{7, http.StatusOK}, // 匹配
		{0, http.StatusOK}, // 未声明，org 以凭证为准
		{8, http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/ingest?org=%d", tc.payloadOrg), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("payloadOrg=%d: got %d want %d", tc.payloadOrg, w.Code, tc.want)
		}
	}
}
