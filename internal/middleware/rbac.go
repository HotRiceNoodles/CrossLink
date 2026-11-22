package middleware

import (
	"net/http"

	"github.com/crosslink/internal/license"
	"github.com/gin-gonic/gin"
)

// Auth-permission degradation flag (audit item 2): set by the JWT middleware
// when the caller's role/status could not be resolved from the DB (lookup
// error other than not-found). Action-gated routes fail CLOSED in that state —
// high-risk operations must not proceed on unverified permission data — while
// endpoints without RequireAction keep serving with the token's bounded
// staleness. Defined here (not in admin) because admin imports this package.
const ctxAuthPermDegraded = "auth_perm_degraded"

// MarkAuthPermDegraded flags the request as having unresolved permission data.
func MarkAuthPermDegraded(c *gin.Context) { c.Set(ctxAuthPermDegraded, true) }

// AuthPermDegraded reports whether permission data was unavailable for this
// request (set by MarkAuthPermDegraded upstream in the middleware chain).
func AuthPermDegraded(c *gin.Context) bool {
	v, ok := c.Get(ctxAuthPermDegraded)
	b, _ := v.(bool)
	return ok && b
}

func getRoleID(c *gin.Context) int64 {
	id, _ := c.Get("role_id")
	if v, ok := id.(int64); ok {
		return v
	}
	return 0
}

// Deprecated: Only used by RequireRole, which is itself deprecated.
func getRoleName(c *gin.Context) string {
	name, _ := c.Get("role_name")
	if v, ok := name.(string); ok {
		return v
	}
	return ""
}

// RequireRole is a coarse-grained role-name check middleware.
// Deprecated: Prefer RequireAction for fine-grained permission control.
// This function is retained for potential future use but no routes currently use it.
func RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		if !allowed[getRoleName(c)] {
			c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireAction checks if the caller's role has the specified action permission.
func RequireAction(cache *PermissionCache, action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Audit item 2: when the JWT middleware could not resolve the caller's
		// role/status from the DB, action-gated (high-risk) operations fail
		// closed instead of trusting unverified permission data.
		if AuthPermDegraded(c) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "permission data unavailable, retry shortly", "error_code": "auth_degraded"})
			c.Abort()
			return
		}
		if !cache.HasPermission(getRoleID(c), action) {
			c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			c.Abort()
			return
		}
		if !license.TierAllowsAction(action) {
			c.JSON(http.StatusForbidden, gin.H{
				"error":       "feature not available in current edition",
				"code":        "TIER_REQUIRED",
				"tier_needed": tierNeededFor(action),
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

func tierNeededFor(action string) string {
	if _, ok := license.TierActionSet[license.TierCommunity][action]; ok {
		return license.TierCommunity
	}
	if _, ok := license.TierActionSet[license.TierPro][action]; ok {
		return license.TierPro
	}
	return license.TierEnterprise
}
