package guardapp

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// incrExpireScript 与网关限速同一 Lua 语义（internal/middleware/ratelimit.go
// 的 incrExpireScript；guardapp 不引 gateway 中间件包，脚本原样复用）：
// 原子自增并在首次自增时设 TTL，返回自增后计数。
var incrExpireScript = redis.NewScript(`
	local count = redis.call('INCR', KEYS[1])
	if count == 1 then
		redis.call('EXPIRE', KEYS[1], ARGV[1])
	end
	return count
`)

// IngestRateLimiter 是 ingest 两级限速（设计 DD）：token 级 + org 级，
// 分钟固定窗口。Redis 可用时计数走 Redis（多节点共享窗口）；不可用时
// 降级为进程内**全局**固定窗口（以 org_rpm 为全局上限——比按 key 的内存
// 表安全，伪造 token 不能撑爆内存）并告警，绝不阻塞请求路径。
type IngestRateLimiter struct {
	rdb      *redis.Client
	tokenRPM int
	orgRPM   int

	// 进程内降级窗口
	mu          sync.Mutex
	windowStart int64 // unix 分钟
	count       int64

	degraded atomic.Bool // Redis 不可用状态（指标/日志用）
}

func NewIngestRateLimiter(rdb *redis.Client, tokenRPM, orgRPM int) *IngestRateLimiter {
	return &IngestRateLimiter{rdb: rdb, tokenRPM: tokenRPM, orgRPM: orgRPM}
}

// Allow 报告本次 ingest 是否放行。两级任一超限即拒绝。
func (l *IngestRateLimiter) Allow(ctx context.Context, tokenID, orgID int64) bool {
	if l.rdb != nil {
		ok, err := l.allowRedis(ctx, tokenID, orgID)
		if err == nil {
			l.degraded.Store(false)
			return ok
		}
		if !l.degraded.Swap(true) {
			slog.Warn("guard ingest rate limiter degraded to in-process global window (Redis unavailable)",
				"error", err)
		}
	}
	return l.allowFallback(time.Now())
}

func (l *IngestRateLimiter) allowRedis(ctx context.Context, tokenID, orgID int64) (bool, error) {
	if l.tokenRPM > 0 {
		count, err := incrExpireScript.Run(ctx, l.rdb,
			[]string{redisKey("guard:rl:token", tokenID)}, 60).Int64()
		if err != nil {
			return false, err
		}
		if count > int64(l.tokenRPM) {
			return false, nil
		}
	}
	if l.orgRPM > 0 {
		count, err := incrExpireScript.Run(ctx, l.rdb,
			[]string{redisKey("guard:rl:org", orgID)}, 60).Int64()
		if err != nil {
			return false, err
		}
		if count > int64(l.orgRPM) {
			return false, nil
		}
	}
	return true, nil
}

// allowFallback 进程内全局固定窗口（分钟粒度），上限取 org_rpm。
func (l *IngestRateLimiter) allowFallback(now time.Time) bool {
	minute := now.Unix() / 60
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windowStart != minute {
		l.windowStart = minute
		l.count = 0
	}
	l.count++
	return l.orgRPM <= 0 || l.count <= int64(l.orgRPM)
}

func redisKey(prefix string, id int64) string {
	return prefix + ":" + strconv.FormatInt(id, 10)
}

// Middleware 包一层 gin 处理器：两级限速，超限 429 + Retry-After。
func (l *IngestRateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenID := c.GetInt64("guard_token_id")
		orgID := c.GetInt64("org_id")
		if l.Allow(c.Request.Context(), tokenID, orgID) {
			c.Next()
			return
		}
		c.Header("Retry-After", "60")
		c.Header("X-RateLimit-Limit", strconv.Itoa(max(l.tokenRPM, l.orgRPM)))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": gin.H{"code": "rate_limit_exceeded", "message": "ingest rate limit exceeded"},
		})
		c.Abort()
	}
}
