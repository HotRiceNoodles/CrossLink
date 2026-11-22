package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
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

const defaultAuthFailPrefix = "auth_fail:"

// authFailKey resolves the Redis key for an IP's auth-failure counter, applying
// the default prefix when the caller passes none.
func authFailKey(keyPrefix, ip string) string {
	if keyPrefix == "" {
		keyPrefix = defaultAuthFailPrefix
	}
	return keyPrefix + ip
}

// ── Audit item 10: Redis-independent fallback ──
//
// AuthFailureLimit used to fail fully OPEN on a Redis error (and do nothing at
// all when Redis was not configured): the credential check still worked, but
// the failure-throttling layer vanished exactly when the shared state store
// was down. The in-process bucket below mirrors the Redis counter with the
// same fixed-window semantics, per process: RecordAuthFailure ALWAYS bumps it
// (Redis too, when reachable), ClearAuthFailures ALWAYS resets it, and
// AuthFailureLimit consults it whenever Redis is unavailable or errors. An
// attacker must now also defeat every gateway process's local state.
type authFailBucket struct {
	count     int
	windowEnd time.Time
}

type authFailFallbackStore struct {
	mu     sync.Mutex
	ips    map[string]*authFailBucket
	capped int64 // last epoch-secs the map was reset for size
}

const authFailFallbackMaxIPs = 100_000

var authFailFallback = &authFailFallbackStore{ips: make(map[string]*authFailBucket)}

func (s *authFailFallbackStore) incr(ip string, window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ips) >= authFailFallbackMaxIPs {
		// Bounded memory: under a spoofed-IP flood the fallback map is reset
		// wholesale — Redis (the real counter) is unaffected.
		s.ips = make(map[string]*authFailBucket)
	}
	now := time.Now()
	b, ok := s.ips[ip]
	if !ok || now.After(b.windowEnd) {
		b = &authFailBucket{windowEnd: now.Add(window)}
		s.ips[ip] = b
	}
	b.count++
	return b.count
}

func (s *authFailFallbackStore) get(ip string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.ips[ip]
	if !ok || time.Now().After(b.windowEnd) {
		return 0
	}
	return b.count
}

func (s *authFailFallbackStore) clear(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ips, ip)
}

// authFailWarnAt throttles the Redis-degraded warn to one line per minute —
// alerting must be observable, not log-flooding (audit item 10).
var authFailWarnAt atomic.Int64

func warnAuthFailRedis(err error) {
	now := time.Now().UnixMilli()
	last := authFailWarnAt.Load()
	if now-last < 60_000 {
		return
	}
	if authFailWarnAt.CompareAndSwap(last, now) {
		slog.Warn("auth-failure limiter: Redis unavailable — serving from in-process fallback counters", "error", err)
	}
}

// AuthFailureLimit blocks requests from an IP once it has accumulated too many
// authentication failures. Read-only: it does not increment the counter, so legit
// requests are not counted as failures. RecordAuthFailure does the increment on
// the actual failure path. Mount it BEFORE the Auth middleware so blocked IPs
// never reach the credential check (audit item 10).
func AuthFailureLimit(rdb *redis.Client, maxFailures int, window time.Duration, keyPrefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxFailures <= 0 {
			c.Next()
			return
		}
		ip := c.ClientIP()
		if rdb == nil {
			// No Redis configured: in-process fallback still throttles.
			if authFailFallback.get(ip) >= maxFailures {
				tooManyAuthFailures(c)
				return
			}
			c.Next()
			return
		}
		count, err := rdb.Get(c.Request.Context(), authFailKey(keyPrefix, ip)).Int()
		if err != nil {
			if err != redis.Nil {
				warnAuthFailRedis(err)
				// Audit item 10: Redis down — fall back to the per-process
				// counter instead of failing the whole limiter open.
				if authFailFallback.get(ip) >= maxFailures {
					tooManyAuthFailures(c)
					return
				}
			}
			c.Next()
			return
		}
		if count >= maxFailures {
			tooManyAuthFailures(c)
			return
		}
		c.Next()
	}
}

func tooManyAuthFailures(c *gin.Context) {
	c.JSON(http.StatusTooManyRequests, gin.H{
		"type":  "error",
		"error": gin.H{"type": "rate_limit_error", "message": "too many failed authentication attempts"},
	})
	c.Abort()
}

// RecordAuthFailure increments the auth-failure counter for an IP. This is the
// single incrementer; call it only when authentication actually fails. Bumps
// BOTH the Redis counter and the in-process fallback (audit item 10).
func RecordAuthFailure(rdb *redis.Client, ip string, window time.Duration, keyPrefix string) {
	authFailFallback.incr(ip, window)
	if rdb == nil {
		return
	}
	authFailIncrScript.Run(context.Background(), rdb, []string{authFailKey(keyPrefix, ip)}, int(window.Seconds()))
}

// ClearAuthFailures resets the auth failure counter for an IP (on success),
// in Redis AND the in-process fallback.
func ClearAuthFailures(rdb *redis.Client, ip string, keyPrefix string) {
	authFailFallback.clear(ip)
	if rdb == nil {
		return
	}
	rdb.Del(context.Background(), authFailKey(keyPrefix, ip))
}
