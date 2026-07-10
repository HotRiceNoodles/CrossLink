package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// authFailIncrScript atomically increments the auth-failure counter and sets the
// TTL only on the first failure. Incrementing is the sole responsibility of
// RecordAuthFailure (called on the failure path); AuthFailureLimit only reads.
var authFailIncrScript = redis.NewScript(`
	local n = redis.call('INCR', KEYS[1])
	if n == 1 then
		redis.call('EXPIRE', KEYS[1], ARGV[1])
	end
	return n
`)

// AuthFailureLimit blocks requests from an IP once it has accumulated too many
// authentication failures. Read-only: it does not increment the counter, so legit
// requests are not counted as failures. RecordAuthFailure does the increment on
// the actual failure path.
func AuthFailureLimit(rdb *redis.Client, maxFailures int, window time.Duration, keyPrefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rdb == nil || maxFailures <= 0 {
			c.Next()
			return
		}
		if keyPrefix == "" {
			keyPrefix = "auth_fail:"
		}
		count, err := rdb.Get(c.Request.Context(), keyPrefix+c.ClientIP()).Int()
		if err == nil && count >= maxFailures {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"type":  "error",
				"error": gin.H{"type": "rate_limit_error", "message": "too many failed authentication attempts"},
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RecordAuthFailure increments the auth-failure counter for an IP. This is the
// single incrementer; call it only when authentication actually fails.
func RecordAuthFailure(rdb *redis.Client, ip string, window time.Duration, keyPrefix string) {
	if rdb == nil {
		return
	}
	if keyPrefix == "" {
		keyPrefix = "auth_fail:"
	}
	authFailIncrScript.Run(context.Background(), rdb, []string{keyPrefix + ip}, int(window.Seconds()))
}

// ClearAuthFailures resets the auth failure counter for an IP (on success).
func ClearAuthFailures(rdb *redis.Client, ip string, keyPrefix string) {
	if rdb == nil {
		return
	}
	if keyPrefix == "" {
		keyPrefix = "auth_fail:"
	}
	rdb.Del(context.Background(), keyPrefix+ip)
}
