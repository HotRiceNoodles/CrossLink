package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// authFailCheckScript atomically checks failure count and increments.
var authFailCheckScript = redis.NewScript(`
	local count = redis.call('GET', KEYS[1])
	if count and tonumber(count) >= tonumber(ARGV[1]) then
		return tonumber(count)
	end
	local new_count = redis.call('INCR', KEYS[1])
	if new_count == 1 then
		redis.call('EXPIRE', KEYS[1], ARGV[2])
	end
	return new_count
`)

// AuthFailureLimit creates middleware that blocks requests from IPs
// with too many recent authentication failures.
func AuthFailureLimit(rdb *redis.Client, maxFailures int, window time.Duration, keyPrefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rdb == nil || maxFailures <= 0 {
			c.Next()
			return
		}

		if keyPrefix == "" {
			keyPrefix = "auth_fail:"
		}
		ip := c.ClientIP()
		key := keyPrefix + ip

		count, err := authFailCheckScript.Run(c.Request.Context(), rdb, []string{key}, maxFailures, int(window.Seconds())).Int64()
		if err != nil {
			c.Next()
			return
		}

		if count > int64(maxFailures) {
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

// RecordAuthFailure increments the auth failure counter for an IP.
func RecordAuthFailure(rdb *redis.Client, ip string, maxFailures int, window time.Duration, keyPrefix string) {
	if rdb == nil {
		return
	}
	if keyPrefix == "" {
		keyPrefix = "auth_fail:"
	}
	key := keyPrefix + ip
	ctx := context.Background()
	authFailCheckScript.Run(ctx, rdb, []string{key}, maxFailures, int(window.Seconds()))
}

// ClearAuthFailures resets the auth failure counter for an IP.
func ClearAuthFailures(rdb *redis.Client, ip string, keyPrefix string) {
	if rdb == nil {
		return
	}
	if keyPrefix == "" {
		keyPrefix = "auth_fail:"
	}
	rdb.Del(context.Background(), keyPrefix+ip)
}

