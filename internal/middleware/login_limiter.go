package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// loginCheckAndIncrScript atomically checks the current count and increments if under the limit.
// This eliminates the TOCTOU window between GET and INCR in the previous implementation.
// Returns the new count after increment.
var loginCheckAndIncrScript = redis.NewScript(`
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

// LoginRateLimit creates middleware that limits failed login attempts per IP.
// Uses an atomic Lua script to check-and-increment before the handler runs,
// eliminating the TOCTOU race between GET and INCR. On success the key is deleted.
func LoginRateLimit(rdb *redis.Client, maxAttempts int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rdb == nil || maxAttempts <= 0 {
			c.Next()
			return
		}

		ip := c.ClientIP()
		key := "login_limit:" + ip
		ctx := context.Background()

		// Atomically check limit and increment. This reserves a slot before the handler.
		result, err := loginCheckAndIncrScript.Run(ctx, rdb, []string{key}, maxAttempts, int(window.Seconds())).Int64()
		if err != nil {
			// Redis error: fail open
			c.Next()
			return
		}

		if result > int64(maxAttempts) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts, please try again later"})
			c.Abort()
			return
		}

		// Wrap response writer to capture status code
		blw := &bodylessWriter{ResponseWriter: c.Writer, statusCode: http.StatusOK}
		c.Writer = blw

		c.Next()

		// On success, delete the key (releases the slot and resets the counter).
		// On failure, the increment already happened atomically above — nothing to do.
		if blw.statusCode == http.StatusOK {
			rdb.Del(ctx, key)
		}
	}
}

// bodylessWriter wraps gin.ResponseWriter to capture the status code
// without intercepting the body.
type bodylessWriter struct {
	gin.ResponseWriter
	statusCode int
}

func (w *bodylessWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodylessWriter) WriteHeaderNow() {
	w.statusCode = w.ResponseWriter.Status()
	w.ResponseWriter.WriteHeaderNow()
}
