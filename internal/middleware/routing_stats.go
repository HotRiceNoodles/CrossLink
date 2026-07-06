package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// recordStatsScript atomically increments all fields of a per-minute routing
// stats hash and sets a TTL on first write (when the key has no expiry yet).
//
// KEYS[1] = hash key, e.g. stats:{org}:{model}:{provider}:{minute}
// ARGV[1] = ttl seconds
// ARGV[2..8] = total, success, err4xx, err5xx, rate_limited, in_tok, out_tok (int deltas)
// ARGV[9]    = cost delta (float)
var recordStatsScript = redis.NewScript(`
	local ttl = tonumber(ARGV[1])
	redis.call('HINCRBY', KEYS[1], 'total',        ARGV[2])
	redis.call('HINCRBY', KEYS[1], 'success',      ARGV[3])
	redis.call('HINCRBY', KEYS[1], 'err4xx',       ARGV[4])
	redis.call('HINCRBY', KEYS[1], 'err5xx',       ARGV[5])
	redis.call('HINCRBY', KEYS[1], 'rate_limited', ARGV[6])
	redis.call('HINCRBY', KEYS[1], 'in_tok',       ARGV[7])
	redis.call('HINCRBY', KEYS[1], 'out_tok',      ARGV[8])
	redis.call('HINCRBYFLOAT', KEYS[1], 'cost',    ARGV[9])
	if redis.call('TTL', KEYS[1]) == -1 then
		redis.call('EXPIRE', KEYS[1], ttl)
	end
	return 1
`)

// RoutingStats is a tail middleware that records per (org, model, provider, minute)
// routing stats into a Redis hash. It runs after c.Next(), reading values that the
// LLM handlers set on the gin.Context: model, provider, input_tokens, output_tokens,
// input_price, output_price, org_id.
//
// Fail-open: any Redis error is logged and the request is not affected.
//
// Latency percentile bucketing is deferred to P2 (DataLens); here we only record
// counters that enable "actual distribution vs configured weight" observability.
// This is the shared data foundation for design 2026-07-06 P2/P3/P4.
func RoutingStats(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if rdb == nil {
			return
		}

		// Only LLM proxy paths set both model and provider on the context.
		modelName, _ := c.Get("model")
		model, _ := modelName.(string)
		providerName, _ := c.Get("provider")
		provider, _ := providerName.(string)
		if model == "" || provider == "" {
			return
		}

		orgID := c.GetInt64("org_id")

		inTok, _ := c.Get("input_tokens")
		outTok, _ := c.Get("output_tokens")
		inPrice, _ := c.Get("input_price")
		outPrice, _ := c.Get("output_price")
		inputTokens, _ := inTok.(int)
		outputTokens, _ := outTok.(int)
		inputPrice, _ := inPrice.(float64)
		outputPrice, _ := outPrice.(float64)
		// Prices are per-1000-tokens (mirrors budget_report.go:53).
		cost := inputPrice*float64(inputTokens)/1000 + outputPrice*float64(outputTokens)/1000

		// Classify the outcome. 429 is counted as rate_limited (separate from err4xx).
		var success, err4xx, err5xx, rateLimited int64
		switch status := c.Writer.Status(); {
		case status >= 200 && status < 300:
			success = 1
		case status == 429:
			rateLimited = 1
		case status >= 400 && status < 500:
			err4xx = 1
		case status >= 500:
			err5xx = 1
		}

		minute := time.Now().Unix() / 60
		key := fmt.Sprintf("stats:%d:%s:%s:%d", orgID, model, provider, minute)

		// Async: observability must never block or delay the response, even when
		// Redis is slow or down (true fail-open). Mirrors UsageLog's goroutine
		// pattern (usage_log.go:43). The pool dial-retry on a dead Redis would
		// otherwise add ~2s per request.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("routing stats goroutine panic", "error", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := recordStatsScript.Run(ctx, rdb, []string{key},
				600, // ttl seconds (~10 windows)
				1, success, err4xx, err5xx, rateLimited,
				inputTokens, outputTokens,
				cost,
			).Result(); err != nil {
				slog.Warn("routing stats redis record failed", "key", key, "error", err)
			}
		}()
	}
}
