package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/license"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runOrgScopeGuard runs one request through a prep middleware (simulating the
// identity/org context the JWT middleware would set), OrgScopeGuard, and a
// downstream sentinel — returning (status, reachedDownstream).
func runOrgScopeGuard(t *testing.T, method, path string, prep func(*gin.Context)) (int, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reached := false
	r := gin.New()
	r.Use(func(c *gin.Context) { prep(c); c.Next() })
	r.Use(OrgScopeGuard())
	r.Handle(method, "/admin/api/*any", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, reached
}

// Acceptance boundary 1 (audit items 1/2): an Enterprise non-platform caller
// without an org context must be denied on EVERY tenant business route — the
// repo layer treats org_id=0 as "no filter" (model:key:usage widen to global).
func TestOrgScopeGuard_EnterpriseOrglessDeniedOnBusinessRoutes(t *testing.T) {
	withTier(t, license.TierEnterprise)
	for _, path := range []string{
		"/admin/api/models",
		"/admin/api/keys",
		"/admin/api/usage",
		"/admin/api/providers",
		"/admin/api/teams",
		"/admin/api/behavior/anomalies",
	} {
		code, reached := runOrgScopeGuard(t, http.MethodGet, path, func(c *gin.Context) {
			setAdminContext(c, 7, 9, "viewer")
			c.Set("org_id", int64(0))
		})
		assert.Equal(t, http.StatusForbidden, code, "path %s", path)
		assert.False(t, reached, "business route %s must not reach the handler", path)
	}
}

// Self-service endpoints stay reachable for the same orgless caller.
func TestOrgScopeGuard_SelfServiceStillReachable(t *testing.T) {
	withTier(t, license.TierEnterprise)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/user/preferences"},
		{http.MethodPut, "/admin/api/user/preferences"},
		{http.MethodGet, "/admin/api/user/profile"},
		{http.MethodPut, "/admin/api/user/profile"},
	} {
		code, reached := runOrgScopeGuard(t, tc.method, tc.path, func(c *gin.Context) {
			setAdminContext(c, 7, 9, "viewer")
			c.Set("org_id", int64(0))
		})
		assert.Equal(t, http.StatusOK, code, "%s %s", tc.method, tc.path)
		assert.True(t, reached, "self-service %s must stay reachable", tc.path)
	}
}

// Enterprise callers WITH an org context are unaffected.
func TestOrgScopeGuard_EnterpriseOrgContextAllowed(t *testing.T) {
	withTier(t, license.TierEnterprise)
	code, reached := runOrgScopeGuard(t, http.MethodGet, "/admin/api/models", func(c *gin.Context) {
		setAdminContext(c, 7, 9, "viewer")
		c.Set("org_id", int64(300))
	})
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, reached)
}

// The platform admin keeps the explicit platform (global) scope.
func TestOrgScopeGuard_PlatformAdminAllowed(t *testing.T) {
	withTier(t, license.TierEnterprise)
	code, reached := runOrgScopeGuard(t, http.MethodGet, "/admin/api/models", func(c *gin.Context) {
		setAdminContext(c, 1, 1, "admin")
		c.Set("org_id", int64(0))
	})
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, reached)
}

// Community/Pro: org_id=0 is the single-tenant norm — no denial (old-data /
// single-tenant compatibility).
func TestOrgScopeGuard_CommunityUnaffected(t *testing.T) {
	withTier(t, license.TierCommunity)
	code, reached := runOrgScopeGuard(t, http.MethodGet, "/admin/api/models", func(c *gin.Context) {
		setAdminContext(c, 7, 9, "viewer")
		c.Set("org_id", int64(0))
	})
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, reached, "community orgless member must keep admin-surface access")
}
