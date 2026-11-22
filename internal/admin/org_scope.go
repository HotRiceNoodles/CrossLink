package admin

import (
	"net/http"
	"strings"

	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/middleware"
	"github.com/gin-gonic/gin"
)

// orgScopeSelfServicePaths are admin-surface routes an orgless user still
// needs for self-service. They carry no tenant business data (own profile /
// preferences only) and must survive the org-scope guard below.
var orgScopeSelfServicePaths = []string{
	"/admin/api/user/preferences",
	"/admin/api/user/profile",
}

func isOrgScopeSelfServicePath(path string) bool {
	for _, allowed := range orgScopeSelfServicePaths {
		if path == allowed || strings.HasPrefix(path, allowed+"/") {
			return true
		}
	}
	return false
}

// OrgScopeGuard is the unified tenant-scope admission for the admin surface
// (audit item 2 extension + final boundary): an Enterprise caller that is NOT
// the platform identity but carries no org context (e.g. lost their last org
// membership; org_id resolves 0) must not fall into any tenant business query
// — the repo layer treats org_id=0 as "no filter", which silently widens
// model/key/usage/provider queries to the GLOBAL catalog (the
// provider-handler fix covered one surface; this guard covers the whole admin
// group).
//
// Behavior:
//   - Community/Pro: never denies (org_id=0 is the single-tenant norm,
//     membership backfilled by ensureDefaultOrganization).
//   - Enterprise platform admin (role admin): never denies (explicit
//     platform scope).
//   - Enterprise orgless non-admin: business routes → 403 org_scope_required;
//     the self-service paths above stay reachable.
//   - Degraded authorization data (middleware.AuthPermDegraded — user/role/
//     team/org-status queries failed, audit item 2 final boundary): business
//     routes → 503 auth_degraded REGARDLESS of the claims' carried org scope
//     (stale claim org must never grant tenant/platform scope); self-service
//     stays reachable (settled behavior).
//
// Mount after OrgResolve. Handler-level denyNoOrgScope checks remain as
// defense in depth for direct handler calls.
func OrgScopeGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		// Audit item 2 final boundary: unresolved authorization data denies
		// business scope outright — whatever org/role the token claims.
		if middleware.AuthPermDegraded(c) {
			if isOrgScopeSelfServicePath(path) {
				c.Next()
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "authorization data unavailable, retry shortly",
				"error_code": "auth_degraded",
			})
			c.Abort()
			return
		}
		if !denyNoOrgScope(c) {
			c.Next()
			return
		}
		if isOrgScopeSelfServicePath(path) {
			c.Next()
			return
		}
		c.JSON(http.StatusForbidden, gin.H{
			"error":      "no organization context: switch to an organization or contact an administrator",
			"error_code": "org_scope_required",
		})
		c.Abort()
	}
}

// denyNoOrgScope reports Enterprise callers that are NOT the platform identity
// yet carry no org context (audit item 2): a non-admin who lost their last org
// membership resolves to org_id=0, and the repo layer treats 0 as "no filter" —
// which silently widens provider queries from tenant-scoped to GLOBAL. The
// platform scope is now explicit: only the platform admin may query globally;
// such callers see nothing. Community/Pro keep org_id=0 as the single-tenant
// norm (ensureDefaultOrganization backfills membership there).
func denyNoOrgScope(c *gin.Context) bool {
	return license.G().CurrentTier() == license.TierEnterprise &&
		GetOrgID(c) == 0 && !IsAdmin(c)
}
