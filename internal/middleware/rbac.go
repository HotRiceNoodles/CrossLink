package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/license"
)

func getRoleID(c *gin.Context) int64 {
	id, _ := c.Get("role_id")
	if v, ok := id.(int64); ok {
		return v
	}
	return 0
}

func getRoleName(c *gin.Context) string {
	name, _ := c.Get("role_name")
	if v, ok := name.(string); ok {
		return v
	}
	return ""
}

// RequireRole creates middleware that restricts access to specified role names
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
