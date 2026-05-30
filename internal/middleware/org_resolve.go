package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
)

// OrgResolve extracts org_id from context (set by JWTAuthMiddleware for admin path)
// or from API key (set by Auth middleware for gateway path).
func OrgResolve() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Admin path: JWTAuthMiddleware already set org_id in context
		if _, exists := c.Get("org_id"); exists {
			c.Next()
			return
		}
		// Gateway path: Auth middleware set api_key in context
		if apiKey, exists := c.Get("api_key"); exists {
			if key, ok := apiKey.(*model.APIKey); ok && key.OrgID != nil {
				c.Set("org_id", *key.OrgID)
			}
		}
		// org_id not set = Super Admin or no Org context, no filtering
		c.Next()
	}
}
