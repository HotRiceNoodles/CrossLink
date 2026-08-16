package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/pkg/token"
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

// reserveTPMAtomicScript atomically reserves tokens and checks the limit.
// If over limit, the reservation is rolled back via DECRBY.
// KEYS[1] = TPM counter key
// ARGV[1] = reservation amount (tokens to reserve)
// ARGV[2] = TPM limit
// ARGV[3] = window TTL in seconds
// Returns: {current_count, 1=allowed or 0=rejected}
var reserveTPMAtomicScript = redis.NewScript(`
	local reservation = tonumber(ARGV[1])
	local limit = tonumber(ARGV[2])
	local ttl = tonumber(ARGV[3])
	local new_count = redis.call('INCRBY', KEYS[1], reservation)
	if new_count == reservation then
		redis.call('EXPIRE', KEYS[1], ttl)
	end
	if new_count > limit then
		redis.call('DECRBY', KEYS[1], reservation)
		return {new_count - reservation, 0}
	end
	return {new_count, 1}
`)

// tpmReservations tracks per-level TPM reservations for the current request.
// Used by ReportTokens to adjust counters from reservation to actual usage.
type tpmReservations struct {
	Key  int // tokens reserved at key level (0 = no reservation)
	Team int // tokens reserved at team level (0 = no reservation)
	Org  int // tokens reserved at org level (0 = no reservation)
}

func RateLimit(rdb *redis.Client, rpm int, teamCache *TeamCache, orgCache *OrgCache) gin.HandlerFunc {
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
					if isRateLimitedWithHeaders(c, rdb, fmt.Sprintf("ratelimit:key:%d", id), apiKey.RPMLimit) {
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
			if isRateLimitedWithHeaders(c, rdb, fmt.Sprintf("ratelimit:%s", limitKey), rpm) {
				return
			}
		}

		// Team-level RPM limiting (API key or JWT admin routes)
		teamID := resolveTeamID(c, apiKey)
		if teamID > 0 {
			team := teamCache.Get(c.Request.Context(), teamID)
			if team != nil && team.RPMLimit > 0 {
				if isRateLimitedWithHeaders(c, rdb, fmt.Sprintf("ratelimit:team:%d", team.ID), team.RPMLimit) {
					return
				}
			}
		}

		// Org-level RPM limiting
		if orgCache != nil {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				org := orgCache.Get(c.Request.Context(), orgID)
				if org != nil && org.RPMLimit > 0 {
					if isRateLimitedWithHeaders(c, rdb, fmt.Sprintf("ratelimit:org:%d", org.ID), org.RPMLimit) {
						return
					}
				}
			}
		}

		c.Next()
	}
}

// estimateTokens estimates the total token count from the request body.
// Returns the sum of estimated input tokens (from messages/prompt content)
// plus an output token estimate (from max_tokens or a default of 1024).
// Returns 0 if the body cannot be parsed.
func estimateTokens(c *gin.Context) int {
	bodyBytes := GetBodyBytes(c)
	if bodyBytes == nil {
		return 0
	}

	// Responses API uses input (string or array) + instructions + max_output_tokens,
	// not messages/max_tokens. Handle it separately.
	if c.Request != nil && strings.HasPrefix(c.Request.URL.Path, "/v1/responses") {
		return estimateResponsesTokens(bodyBytes)
	}

	var probe struct {
		Prompt   string `json:"prompt"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if json.Unmarshal(bodyBytes, &probe) != nil {
		return 0
	}

	total := 0
	if probe.Prompt != "" {
		total += token.Estimate(probe.Prompt)
	}
	for _, m := range probe.Messages {
		if m.Content != "" {
			total += token.Estimate(m.Content)
		}
	}
	if total == 0 {
		return 0
	}

	total += outputTokenReservation(probe.MaxTokens)
	return total
}

// outputTokenReservation estimates the output-token portion of a TPM reservation.
// TPM is a rate-shaping quota reconciled to actual usage by ReportTokens, so
// reserving the full max_tokens (worst case) systematically overestimates
// concurrent in-flight demand and rejects requests whose actual usage fits.
// Reserve half the requested cap, bounded — under-reservation self-corrects at
// settlement, over-reservation causes spurious 429s.
func outputTokenReservation(maxTokens int) int {
	if maxTokens <= 0 {
		return 512
	}
	half := maxTokens / 2
	if half > 8192 {
		half = 8192
	}
	return half
}

// estimateResponsesTokens estimates TPM reservation for a Responses API body.
// input is polymorphic (string or array of message/function_call_output items).
func estimateResponsesTokens(bodyBytes []byte) int {
	var probe struct {
		Input         json.RawMessage `json:"input"`
		Instructions  string          `json:"instructions"`
		MaxOutputTokens int           `json:"max_output_tokens"`
	}
	if json.Unmarshal(bodyBytes, &probe) != nil {
		return 0
	}
	total := 0
	if probe.Instructions != "" {
		total += token.Estimate(probe.Instructions)
	}
	if len(probe.Input) > 0 {
		var s string
		if json.Unmarshal(probe.Input, &s) == nil && probe.Input[0] == '"' {
			total += token.Estimate(s)
		} else {
			var items []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
				Output  string          `json:"output"`
			}
			if json.Unmarshal(probe.Input, &items) == nil {
				for _, it := range items {
					if it.Type == "message" && len(it.Content) > 0 {
						total += token.Estimate(domain.ContentText(it.Content))
					}
					if it.Type == "function_call_output" && it.Output != "" {
						total += token.Estimate(it.Output)
					}
				}
			}
		}
	}
	if total == 0 {
		return 0
	}
	total += outputTokenReservation(probe.MaxOutputTokens)
	return total
}

// isNonTokenEndpoint reports gateway paths whose handlers do not account chat
// tokens (images, audio, embeddings, batch, video). TPMLimit skips these so the
// default 2000-token reservation is not silently leaked per request.
func isNonTokenEndpoint(path string) bool {
	return strings.HasPrefix(path, "/v1/videos") ||
		strings.HasPrefix(path, "/v1/images/") ||
		strings.HasPrefix(path, "/v1/audio/") ||
		strings.HasPrefix(path, "/v1/embeddings") ||
		strings.HasPrefix(path, "/v1/batch") ||
		strings.HasPrefix(path, "/v1/batches")
}

func TPMLimit(rdb *redis.Client, tpm int, teamCache *TeamCache, orgCache *OrgCache, reservationAmount int, failClosed bool) gin.HandlerFunc {
	defaultReservation := reservationAmount
	if defaultReservation <= 0 {
		defaultReservation = 2000
	}

	return func(c *gin.Context) {
		// Skip TPM for non-chat endpoints that have no chat-token accounting.
		// These handlers report 0 tokens; without the skip each request would
		// reserve defaultReservation (2000) tokens that never get reconciled,
		// silently draining the TPM budget.
		if isNonTokenEndpoint(c.Request.URL.Path) {
			c.Set("tpm_key", "")
			c.Set("tpm_reservations", &tpmReservations{})
			c.Next()
			return
		}

		res := &tpmReservations{}
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
			c.Set("tpm_reservations", res)
			c.Next()
			return
		}

		// Use token estimation if available, fallback to default reservation
		reservation := estimateTokens(c)
		if reservation <= 0 || reservation < defaultReservation {
			reservation = defaultReservation
		}

		ctx := c.Request.Context()
		windowTTL := int((2 * time.Minute).Seconds())

		// Key-level TPM: atomic reserve
		result, err := reserveTPMAtomicScript.Run(ctx, rdb, []string{tpmKey}, reservation, limit, windowTTL).Int64Slice()
		if err != nil {
			if failClosed {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"type":  "error",
					"error": gin.H{"type": "server_error", "message": "rate limiter unavailable"},
				})
				c.Abort()
				return
			}
			c.Set("tpm_key", tpmKey)
			c.Set("tpm_reservations", res)
			c.Next()
			return
		}
		if len(result) > 1 && result[1] == 0 {
			abortTPMLimit(c, rdb, tpmKey, limit, "token rate limit exceeded")
			return
		}
		// Set X-RateLimit headers for successful key-level check
		if len(result) > 0 {
			setRateLimitHeaders(c, limit, limit-int(result[0]), rdb, tpmKey)
		}
		res.Key = reservation

		// Team-level TPM limiting (API key or JWT admin routes)
		teamID := resolveTeamID(c, apiKey)
		if teamID > 0 {
			team := teamCache.Get(ctx, teamID)
			if team != nil && team.TPMLimit > 0 {
				teamTPMKey := fmt.Sprintf("tpm:team:%d", team.ID)
				teamResult, teamErr := reserveTPMAtomicScript.Run(ctx, rdb, []string{teamTPMKey}, reservation, team.TPMLimit, windowTTL).Int64Slice()
				if teamErr == nil && len(teamResult) > 1 && teamResult[1] == 0 {
					// Team rejected: refund key-level reservation
					rdb.DecrBy(ctx, tpmKey, int64(reservation))
					res.Key = 0
					abortTPMLimit(c, rdb, teamTPMKey, team.TPMLimit, "team token rate limit exceeded")
					return
				}
				if teamErr == nil {
					res.Team = reservation
				}
			}
		}

		// Org-level TPM limiting
		if orgCache != nil {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				org := orgCache.Get(ctx, orgID)
				if org != nil && org.TPMLimit > 0 {
					orgTPMKey := fmt.Sprintf("tpm:org:%d", org.ID)
					orgResult, orgErr := reserveTPMAtomicScript.Run(ctx, rdb, []string{orgTPMKey}, reservation, org.TPMLimit, windowTTL).Int64Slice()
					if orgErr == nil && len(orgResult) > 1 && orgResult[1] == 0 {
						// Org rejected: refund key and team reservations
						rdb.DecrBy(ctx, tpmKey, int64(reservation))
						res.Key = 0
						if res.Team > 0 && teamID > 0 {
							teamTPMKey := fmt.Sprintf("tpm:team:%d", teamID)
							rdb.DecrBy(ctx, teamTPMKey, int64(reservation))
							res.Team = 0
						}
						abortTPMLimit(c, rdb, orgTPMKey, org.TPMLimit, "organization token rate limit exceeded")
						return
					}
					if orgErr == nil {
						res.Org = reservation
					}
				}
			}
		}

		c.Set("tpm_key", tpmKey)
		c.Set("tpm_reservations", res)
		c.Next()
	}
}

func ReportTokens(rdb *redis.Client, orgCache *OrgCache) gin.HandlerFunc {
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

		// Extract per-level reservations for delta adjustment
		var keyRes, teamRes, orgRes int
		if resVal, ok := c.Get("tpm_reservations"); ok {
			if res, ok := resVal.(*tpmReservations); ok {
				keyRes = res.Key
				teamRes = res.Team
				orgRes = res.Org
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Key level: adjust from reservation to actual usage
		if delta := total - keyRes; delta != 0 {
			if _, err := incrByExpireScript.Run(ctx, rdb, []string{key}, delta, int(time.Minute.Seconds())).Result(); err != nil {
				slog.Warn("report tokens redis atomic incrby failed", "key", key, "error", err)
			}
		}

		// Also report to team TPM counter if applicable
		apiKey := GetAPIKeyFromContext(c)
		teamID := resolveTeamID(c, apiKey)
		if teamID > 0 {
			teamTPMKey := fmt.Sprintf("tpm:team:%d", teamID)
			if delta := total - teamRes; delta != 0 {
				if _, err := incrByExpireScript.Run(ctx, rdb, []string{teamTPMKey}, delta, int(time.Minute.Seconds())).Result(); err != nil {
					slog.Warn("report tokens redis team atomic incrby failed", "key", teamTPMKey, "error", err)
				}
			}
		}

		// Report to org TPM counter
		if orgCache != nil {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				orgTPMKey := fmt.Sprintf("tpm:org:%d", orgID)
				if delta := total - orgRes; delta != 0 {
					if _, err := incrByExpireScript.Run(ctx, rdb, []string{orgTPMKey}, delta, int(time.Minute.Seconds())).Result(); err != nil {
						slog.Warn("report tokens redis org atomic incrby failed", "key", orgTPMKey, "error", err)
					}
				}
			}
		}
	}
}

// --- Internal helpers ---

func isRateLimited(ctx context.Context, rdb *redis.Client, key string, rpm int) bool {
	count, err := incrExpireScript.Run(ctx, rdb, []string{key}, int(time.Minute.Seconds())).Int64()
	if err != nil {
		return false
	}
	return count > int64(rpm)
}

// isRateLimitedWithHeaders checks RPM limit and sets X-RateLimit headers.
// Returns true if rate limited (and aborts the request).
func isRateLimitedWithHeaders(c *gin.Context, rdb *redis.Client, key string, rpm int) bool {
	count, err := incrExpireScript.Run(c.Request.Context(), rdb, []string{key}, int(time.Minute.Seconds())).Int64()
	if err != nil {
		// Redis error: fail-open, no headers
		return false
	}
	if count > int64(rpm) {
		retryAfter := retryAfterFromTTL(c.Request.Context(), rdb, key)
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", rpm))
		c.Header("X-RateLimit-Remaining", "0")
		c.JSON(http.StatusTooManyRequests, gin.H{
			"type":  "error",
			"error": gin.H{"type": "rate_limit_error", "message": "rate limit exceeded"},
		})
		c.Abort()
		return true
	}
	remaining := int64(rpm) - count
	if remaining < 0 {
		remaining = 0
	}
	c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", rpm))
	c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
	return false
}

// abortTPMLimit aborts with a 429 TPM error, using dynamic Retry-After from Redis TTL.
func abortTPMLimit(c *gin.Context, rdb *redis.Client, key string, limit int, message string) {
	retryAfter := retryAfterFromTTL(c.Request.Context(), rdb, key)
	c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
	c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
	c.Header("X-RateLimit-Remaining", "0")
	c.JSON(http.StatusTooManyRequests, gin.H{
		"type":  "error",
		"error": gin.H{"type": "rate_limit_error", "message": message},
	})
	c.Abort()
}

// retryAfterFromTTL returns the remaining seconds until the rate limit window resets.
// Falls back to 60 seconds if TTL lookup fails.
func retryAfterFromTTL(ctx context.Context, rdb *redis.Client, key string) int {
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		return 60
	}
	return int(ttl.Seconds()) + 1
}

// setRateLimitHeaders sets X-RateLimit headers for a successful TPM check.
func setRateLimitHeaders(c *gin.Context, limit, remaining int, rdb *redis.Client, key string) {
	if remaining < 0 {
		remaining = 0
	}
	c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
	c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
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
