package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupAuthRevokeDB builds an in-memory DB shaped for JWTAuthMiddleware:
// users + roles rows. The user carries the admin role so membership lookups
// are skipped (isolate the revocation logic under test).
func setupAuthRevokeDB(t *testing.T) (*gorm.DB, *model.User) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Role{}, &model.TeamMember{}, &model.OrgMember{}, &model.Organization{}))
	role := model.Role{Name: model.RoleAdmin, DisplayName: "admin", IsSystem: true}
	require.NoError(t, db.Create(&role).Error)
	user := &model.User{Username: "revoked-admin", DisplayName: "R", RoleID: role.ID, Status: 1, PasswordHash: "$2a$10$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	require.NoError(t, db.Create(user).Error)
	return db, user
}

func authRevokeCfgCP(t *testing.T) (config.AdminConfig, crypto.CryptoProvider) {
	t.Helper()
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	return config.AdminConfig{JWTSecret: "test-secret", TokenExpiry: 24}, cp
}

func authRevokeRequest(t *testing.T, cfg config.AdminConfig, cp crypto.CryptoProvider, db *gorm.DB, token string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/version", nil)
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: token})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	JWTAuthMiddleware(cfg, db, cp)(c)
	return c, w
}

// Audit item 2: a soft-deleted user's still-unexpired session must be revoked
// at the middleware — not kept alive on token claims.
func TestJWTAuthMiddleware_SoftDeletedUserRevoked(t *testing.T) {
	db, user := setupAuthRevokeDB(t)
	cfg, cp := authRevokeCfgCP(t)
	token, err := GenerateToken(user, model.RoleAdmin, 0, 0, "", cfg, cp)
	require.NoError(t, err)

	// Pre-soft-delete sanity: session passes.
	c, w := authRevokeRequest(t, cfg, cp, db, token)
	assert.False(t, c.IsAborted(), "live user session should pass, got %d", w.Code)

	// Soft delete (GORM default scope hides the row from First → ErrRecordNotFound).
	require.NoError(t, db.Delete(user).Error)

	c2, w2 := authRevokeRequest(t, cfg, cp, db, token)
	assert.True(t, c2.IsAborted())
	assert.Equal(t, http.StatusUnauthorized, w2.Code, "body: %s", w2.Body.String())

	// Hard delete behaves the same.
	require.NoError(t, db.Unscoped().Delete(user).Error)
	_, w3 := authRevokeRequest(t, cfg, cp, db, token)
	assert.Equal(t, http.StatusUnauthorized, w3.Code)
}

// Audit item 2: when the user lookup fails with a DB error (NOT not-found),
// the request is flagged degraded: ungated endpoints keep serving, while
// action-gated (RequireAction) routes fail closed with 503.
func TestJWTAuthMiddleware_DBErrorDegradesButGatedRoutesFailClosed(t *testing.T) {
	db, user := setupAuthRevokeDB(t)
	cfg, cp := authRevokeCfgCP(t)
	token, err := GenerateToken(user, model.RoleAdmin, 0, 0, "", cfg, cp)
	require.NoError(t, err)

	// Kill the underlying connection so lookups fail with a driver error
	// (distinct from ErrRecordNotFound).
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	c, w := authRevokeRequest(t, cfg, cp, db, token)
	assert.False(t, c.IsAborted(), "ungated endpoint must keep serving during DB degradation, got %d: %s", w.Code, w.Body.String())
	assert.True(t, middleware.AuthPermDegraded(c), "degraded flag must be set for action-gated routes")

	// An action-gated route behind the same chain fails closed.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/providers", nil)
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: token})
	w2 := httptest.NewRecorder()
	r := gin.New()
	r.GET("/admin/api/providers", func(c *gin.Context) {
		JWTAuthMiddleware(cfg, db, cp)(c)
		if c.IsAborted() {
			return
		}
		middleware.RequireAction(middleware.NewPermissionCache(nil), "provider:list")(c)
		if c.IsAborted() {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.ServeHTTP(w2, req)
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code, "gated route must fail closed on degraded permission data, body: %s", w2.Body.String())
}

// --- Audit item 2: explicit platform scope for provider queries ---

func setupOrgScopeHandler(t *testing.T) (*ProviderHandler, *mockProviderRepo) {
	t.Helper()
	repo, _ := defaultProviderMocks()
	repo.listFn = func(ctx context.Context, orgID int64) ([]model.Provider, error) {
		return []model.Provider{{ID: 1, Name: "global-provider", Status: 1}}, nil
	}
	return newProviderHandler(repo, nil), repo
}

func withTier(t *testing.T, tier string) {
	t.Helper()
	prev := license.G()
	license.SetGlobalForTest(license.NewGate(&license.VerifiedToken{Tier: tier}))
	t.Cleanup(func() { license.SetGlobalForTest(prev) })
}

// Enterprise non-admin who lost org membership (org_id resolves 0) must get
// an EMPTY provider list — the platform/global scope belongs to the platform
// admin only. The repo must not even be queried.
func TestProviderList_OrglessEnterpriseNonAdminSeesNothing(t *testing.T) {
	withTier(t, license.TierEnterprise)
	h, repo := setupOrgScopeHandler(t)

	called := false
	repo.listFn = func(ctx context.Context, orgID int64) ([]model.Provider, error) {
		called = true
		return []model.Provider{{ID: 1, Name: "leak", Status: 1}}, nil
	}

	c, w := newTestContext(t, http.MethodGet, "/admin/api/providers", nil)
	setAdminContext(c, 7, 9, "viewer") // non-admin role name
	c.Set("org_id", int64(0))          // membership removed → org context zeroed
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, called, "global query must not run for orgless non-admin")
	assert.Contains(t, w.Body.String(), `"data":[]`, "empty list, body: %s", w.Body.String())
}

// The same orgless caller is denied object access (404, non-disclosing) on
// the org-scoped single-provider paths.
func TestProviderUpdate_OrglessEnterpriseNonAdminNotFound(t *testing.T) {
	withTier(t, license.TierEnterprise)
	h, repo := setupOrgScopeHandler(t)
	called := false
	repo.getByIDFn = func(ctx context.Context, orgID, id int64) (*model.Provider, error) {
		called = true
		return &model.Provider{ID: id, Name: "x", Status: 1}, nil
	}

	c, w := newTestContext(t, http.MethodPut, "/admin/api/providers/1", gin.H{"display_name": "x"})
	setAdminContext(c, 7, 9, "viewer")
	c.Set("org_id", int64(0))
	setPathParams(c, gin.Params{{Key: "id", Value: "1"}})
	h.Update(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.False(t, called, "repo must not resolve global objects for orgless non-admin")
}

// Platform admin (role admin, org 0) keeps the global view in Enterprise.
func TestProviderList_PlatformAdminKeepsGlobalView(t *testing.T) {
	withTier(t, license.TierEnterprise)
	h, _ := setupOrgScopeHandler(t)

	c, w := newTestContext(t, http.MethodGet, "/admin/api/providers", nil)
	setAdminContext(c, 1, 1, model.RoleAdmin)
	c.Set("org_id", int64(0))
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "global-provider", "platform admin must keep the global catalog")
}

// Community single-tenant deployments rely on org_id=0 as the norm — the
// explicit-platform rule must NOT change community behavior (old-data compat).
func TestProviderList_CommunityOrglessUnchanged(t *testing.T) {
	withTier(t, license.TierCommunity)
	h, _ := setupOrgScopeHandler(t)

	c, w := newTestContext(t, http.MethodGet, "/admin/api/providers", nil)
	setAdminContext(c, 7, 9, "viewer")
	c.Set("org_id", int64(0))
	h.List(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "global-provider", "community org_id=0 stays the single-tenant norm")
}

// Expired tokens are still rejected before any of the new paths run (guard
// against regressions in the revocation change).
func TestJWTAuthMiddleware_ExpiredTokenRejected(t *testing.T) {
	_, user := setupAuthRevokeDB(t)
	cfg, cp := authRevokeCfgCP(t)
	now := time.Now().Add(-2 * time.Hour)
	claims := Claims{
		UserID: user.ID, Username: user.Username, RoleID: user.RoleID, RoleName: model.RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "expired-session",
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "crosslink",
		},
	}
	tok := jwt.NewWithClaims(cp.JWTSigningMethod(), claims)
	token, err := tok.SignedString([]byte(cfg.JWTSecret))
	require.NoError(t, err)

	_, w := authRevokeRequest(t, cfg, cp, nil, token)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── Audit item 2, final boundary: authorization-adjacent query failures ──

// setupDegradedDB seeds a NON-admin user (viewer role) holding an org
// membership, mirroring the claims a fresh login would carry.
func setupDegradedDB(t *testing.T) (*gorm.DB, *model.User, *model.Organization, config.AdminConfig, crypto.CryptoProvider, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Role{}, &model.TeamMember{}, &model.OrgMember{}, &model.Organization{}))
	role := model.Role{Name: "viewer", DisplayName: "viewer", IsSystem: true}
	require.NoError(t, db.Create(&role).Error)
	org := model.Organization{Name: "org-a", DisplayName: "A", Status: 1}
	require.NoError(t, db.Create(&org).Error)
	user := &model.User{Username: "scoped-user", DisplayName: "S", RoleID: role.ID, Status: 1, PasswordHash: "$2a$10$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(&model.OrgMember{OrgID: org.ID, UserID: user.ID, Role: "member", JoinedAt: time.Now()}).Error)

	cfg := config.AdminConfig{JWTSecret: "test-secret", TokenExpiry: 24}
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	token, err := GenerateToken(user, "viewer", 0, org.ID, "member", cfg, cp)
	require.NoError(t, err)
	return db, user, &org, cfg, cp, token
}

func degradedRequest(t *testing.T, cfg config.AdminConfig, cp crypto.CryptoProvider, db *gorm.DB, token string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: token})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	JWTAuthMiddleware(cfg, db, cp)(c)
	return c, w
}

// 角色名称查不到（行缺失或查询错误）：不得沿用 JWT 旧角色 → 降级。
func TestJWTAuthMiddleware_RoleLookupFailureDegraded(t *testing.T) {
	db, user, _, cfg, cp, token := setupDegradedDB(t)

	// Sanity: with everything intact the request passes undegraded.
	c, _ := degradedRequest(t, cfg, cp, db, token)
	assert.False(t, middleware.AuthPermDegraded(c))

	// Delete the role ROW (not-found) → degraded; stale claim role not trusted.
	require.NoError(t, db.Where("id = ?", user.RoleID).Delete(&model.Role{}).Error)
	c2, _ := degradedRequest(t, cfg, cp, db, token)
	assert.True(t, middleware.AuthPermDegraded(c2), "unresolvable role must degrade authorization")

	// Technical query failure (table gone) → degraded as well.
	require.NoError(t, db.Migrator().DropTable("roles"))
	c3, _ := degradedRequest(t, cfg, cp, db, token)
	assert.True(t, middleware.AuthPermDegraded(c3))
}

// 组织成员关系查询失败（技术错误）：即使 JWT 声明携带 org，也不得获得租户/
// 平台业务范围 → 降级；无成员（not-found）是合法状态，不降级、org 归零。
func TestJWTAuthMiddleware_OrgMembershipQueryFailureDegraded(t *testing.T) {
	db, _, _, cfg, cp, token := setupDegradedDB(t)

	// Not-found = legitimate no-membership: org zeroed, NOT degraded.
	require.NoError(t, db.Where("1=1").Delete(&model.OrgMember{}).Error)
	c, _ := degradedRequest(t, cfg, cp, db, token)
	assert.False(t, middleware.AuthPermDegraded(c), "absent membership is a legitimate state, not an outage")
	assert.Equal(t, int64(0), GetOrgID(c), "no membership → org context zeroed (normal org-scope rules apply)")

	// Technical failure (table gone): claims' org must not carry authorization.
	db2, _, _, cfg2, cp2, token2 := setupDegradedDB(t)
	require.NoError(t, db2.Migrator().DropTable("organization_members"))
	c2, _ := degradedRequest(t, cfg2, cp2, db2, token2)
	assert.True(t, middleware.AuthPermDegraded(c2), "org membership query failure must degrade authorization")
}

// 组织状态查询的技术性失败：停用状态无法核实 → 降级（不得按未停用放行）。
func TestJWTAuthMiddleware_OrgStatusQueryFailureDegraded(t *testing.T) {
	db, _, _, cfg, cp, token := setupDegradedDB(t)
	require.NoError(t, db.Migrator().DropTable("organizations"))
	c, _ := degradedRequest(t, cfg, cp, db, token)
	assert.True(t, middleware.AuthPermDegraded(c), "org status query failure must degrade authorization")
}

// 降级状态下不得获得平台/租户业务范围；自助接口保留（定案行为）。
func TestOrgScopeGuard_DegradedDeniesBusinessKeepsSelfService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reached := false
	r := gin.New()
	r.Use(func(c *gin.Context) { middleware.MarkAuthPermDegraded(c); c.Next() }) // JWT 中间件的降级路径
	r.Use(OrgScopeGuard())
	r.GET("/admin/api/models", func(c *gin.Context) { reached = true; c.Status(http.StatusOK) })
	r.GET("/admin/api/user/preferences", func(c *gin.Context) { reached = true; c.Status(http.StatusOK) })

	// Business route denied even though the context carries an ORG scope
	// (simulating stale claims surviving a failed membership query).
	req := httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "auth_degraded")
	assert.False(t, reached, "degraded caller must not reach tenant business data")

	// Self-service stays reachable (settled behavior).
	reached = false
	req2 := httptest.NewRequest(http.MethodGet, "/admin/api/user/preferences", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.True(t, reached, "self-service must survive the degraded state")
}
