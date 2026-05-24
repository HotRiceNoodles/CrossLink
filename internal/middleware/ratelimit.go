package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/redis/go-redis/v9"
)

// incrExpireScript atomically increments and sets TTL only on first increment.
// Returns the new count after increment.
var incrExpireScript = redis.NewScript(`
	local count = redis.call('INCR', KEYS[1])
	if count == 1 then
		redis.call('EXPIRE', KEYS[1], ARGV[1])
	end
	return count
`)

// incrByExpireScript atomically increments by N and sets TTL only on first increment.
var incrByExpireScript = redis.NewScript(`
	local count = redis.call('INCRBY', KEYS[1], ARGV[1])
	if count == tonumber(ARGV[1]) then
		redis.call('EXPIRE', KEYS[1], ARGV[2])
	end
	return count
`)

// reserveTPMScript atomically checks TPM limit without incrementing.
// Returns 1 if allowed, 0 if rejected.
var reserveTPMScript = redis.NewScript(`
	local current = tonumber(redis.call('GET', KEYS[1]) or '0')
	local limit = tonumber(ARGV[1])
	if current >= limit then
		return 0
	end
	return 1
`)

func RateLimit(rdb *redis.Client, rpm int, teamCache *TeamCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rpm <= 0 {
			c.Next()
			return
		}

		// Per-key rate limiting: use api_key_id if available, fallback to IP
		// WARNING: c.ClientIP() trusts X-Forwarded-For when TrustedProxies is configured.
		// Ensure gin.Engine.SetTrustedProxies() is set correctly to prevent IP spoofing
		// that could bypass rate limits. When TrustedProxies is empty, ClientIP() uses
		// the direct connection IP (RemoteAddr), which is safe.
		limitKey := c.ClientIP()
		apiKey := GetAPIKeyFromContext(c)
		if keyID, exists := c.Get("api_key_id"); exists {
			if id, ok := keyID.(int64); ok && id > 0 {
				if apiKey != nil && apiKey.RPMLimit > 0 {
					if isRateLimited(c.Request.Context(), rdb, fmt.Sprintf("ratelimit:key:%d", id), apiKey.RPMLimit) {
						abortRateLimit(c)
						return
					}
				} else {
					limitKey = fmt.Sprintf("key:%d", id)
				}
			}
		}

		// Admin routes (JWT auth): use user_id instead of IP
		if apiKey == nil {
			if userID, exists := c.Get("user_id"); exists {
				if id, ok := userID.(int64); ok && id > 0 {
					limitKey = fmt.Sprintf("admin:%d", id)
				}
			}
		}

		// Global per-key/IP rate limiting (only if per-key override didn't apply)
		if apiKey == nil || apiKey.RPMLimit <= 0 {
			if isRateLimited(c.Request.Context(), rdb, fmt.Sprintf("ratelimit:%s", limitKey), rpm) {
				abortRateLimit(c)
				return
			}
		}

		// Team-level RPM limiting (API key or JWT admin routes)
		teamID := resolveTeamID(c, apiKey)
		if teamID > 0 {
			team := teamCache.Get(c.Request.Context(), teamID)
			if team != nil && team.RPMLimit > 0 {
				if isRateLimited(c.Request.Context(), rdb, fmt.Sprintf("ratelimit:team:%d", team.ID), team.RPMLimit) {
					abortRateLimit(c)
					return
				}
			}
		}

		c.Next()
	}
}

func TPMLimit(rdb *redis.Client, tpm int, teamCache *TeamCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := tpm
		// WARNING: See RateLimit() comment about TrustedProxies and ClientIP().
		tpmKey := c.ClientIP()
		apiKey := GetAPIKeyFromContext(c)
		if apiKey != nil && apiKey.TPMLimit > 0 {
			limit = apiKey.TPMLimit
		}
		if keyID, exists := c.Get("api_key_id"); exists {
			if id, ok := keyID.(int64); ok && id > 0 {
				tpmKey = fmt.Sprintf("tpm:key:%d", id)
			}
		}

		// Admin routes (JWT auth): use user_id instead of IP
		if apiKey == nil {
			if userID, exists := c.Get("user_id"); exists {
				if id, ok := userID.(int64); ok && id > 0 {
					tpmKey = fmt.Sprintf("tpm:admin:%d", id)
				}
			}
		}

		if limit <= 0 {
			c.Set("tpm_key", tpmKey)
			c.Next()
			return
		}

		ctx := c.Request.Context()
		allowed, err := reserveTPMScript.Run(ctx, rdb, []string{tpmKey}, limit).Int64()
		if err != nil {
			c.Set("tpm_key", tpmKey)
			c.Next()
			return
		}
		if allowed == 0 {
			c.Header("Retry-After", "60")
			c.JSON(http.StatusTooManyRequests, gin.H{
				"type":  "error",
				"error": gin.H{"type": "rate_limit_error", "message": "token rate limit exceeded"},
			})
			c.Abort()
			return
		}
		rdb.Expire(ctx, tpmKey, 2*time.Minute)

		// Team-level TPM limiting (API key or JWT admin routes)
		teamID := resolveTeamID(c, apiKey)
		if teamID > 0 {
			team := teamCache.Get(ctx, teamID)
			if team != nil && team.TPMLimit > 0 {
				teamTPMKey := fmt.Sprintf("tpm:team:%d", team.ID)
				teamAllowed, teamErr := reserveTPMScript.Run(ctx, rdb, []string{teamTPMKey}, team.TPMLimit).Int64()
				if teamErr == nil && teamAllowed == 0 {
					c.Header("Retry-After", "60")
					c.JSON(http.StatusTooManyRequests, gin.H{
						"type":  "error",
						"error": gin.H{"type": "rate_limit_error", "message": "team token rate limit exceeded"},
					})
					c.Abort()
					return
				}
			}
		}

		c.Set("tpm_key", tpmKey)
		c.Next()
	}
}

func ReportTokens(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		tpmKey, exists := c.Get("tpm_key")
		if !exists {
			return
		}
		key := tpmKey.(string)

		inputTokens, _ := c.Get("input_tokens")
		outputTokens, _ := c.Get("output_tokens")
		total := 0
		if v, ok := inputTokens.(int); ok {
			total += v
		}
		if v, ok := outputTokens.(int); ok {
			total += v
		}
		if total <= 0 {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if _, err := incrByExpireScript.Run(ctx, rdb, []string{key}, total, int(time.Minute.Seconds())).Result(); err != nil {
			slog.Warn("report tokens redis atomic incrby failed", "key", key, "error", err)
		}

		// Also report to team TPM counter if applicable
		apiKey := GetAPIKeyFromContext(c)
		teamID := resolveTeamID(c, apiKey)
		if teamID > 0 {
			teamTPMKey := fmt.Sprintf("tpm:team:%d", teamID)
			if _, err := incrByExpireScript.Run(ctx, rdb, []string{teamTPMKey}, total, int(time.Minute.Seconds())).Result(); err != nil {
				slog.Warn("report tokens redis team atomic incrby failed", "key", teamTPMKey, "error", err)
			}
		}
	}
}

func isRateLimited(ctx context.Context, rdb *redis.Client, key string, rpm int) bool {
	count, err := incrExpireScript.Run(ctx, rdb, []string{key}, int(time.Minute.Seconds())).Int64()
	if err != nil {
		return false
	}
	return count > int64(rpm)
}

func abortRateLimit(c *gin.Context) {
	c.Header("Retry-After", "60")
	c.JSON(http.StatusTooManyRequests, gin.H{
		"type":  "error",
		"error": gin.H{"type": "rate_limit_error", "message": "rate limit exceeded"},
	})
	c.Abort()
}

// resolveTeamID returns the team ID from API key context or JWT admin context.
func resolveTeamID(c *gin.Context, apiKey *model.APIKey) int64 {
	if apiKey != nil && apiKey.TeamID != nil {
		return *apiKey.TeamID
	}
	if tid, exists := c.Get("team_id"); exists {
		if id, ok := tid.(int64); ok {
			return id
		}
	}
	return 0
}
