package middleware

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// AdminRateLimit limits admin API requests per user (from JWT user_id).
// Falls back to client IP if user_id is not set. Passes through if rdb is nil or rpm <= 0.
func AdminRateLimit(rdb *redis.Client, rpm int, window time.Duration, keyPrefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rdb == nil || rpm <= 0 {
			c.Next()
			return
		}

		limitKey := c.ClientIP()
		if userID, exists := c.Get("user_id"); exists {
			if id, ok := userID.(int64); ok && id > 0 {
				limitKey = fmt.Sprintf("admin:%d", id)
			}
		}

		if keyPrefix == "" {
			keyPrefix = "admin_ratelimit:"
		}
		redisKey := keyPrefix + limitKey

		if isRateLimited(c.Request.Context(), rdb, redisKey, rpm) {
			c.Header("Retry-After", "60")
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "admin rate limit exceeded",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}
