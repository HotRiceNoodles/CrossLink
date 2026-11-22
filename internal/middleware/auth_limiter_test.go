package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestAuthFailureLimit_GetOnlyNoIncrement verifies the cleanup: AuthFailureLimit
// must NOT increment the counter (previously it ran an INCR script on every
// request, counting legit traffic as failures). Only RecordAuthFailure increments.
func TestAuthFailureLimit_GetOnlyNoIncrement(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	defer mr.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthFailureLimit(rdb, 5, time.Minute, ""))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Pass 3 requests through — counter must stay 0 (no increment on the check path).
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	count, err := rdb.Get(ctxBG(), "auth_fail:127.0.0.1").Int()
	if err == nil && count != 0 {
		t.Fatalf("AuthFailureLimit must not increment; counter = %d", count)
	}

	// RecordAuthFailure is the sole incrementer.
	RecordAuthFailure(rdb, "127.0.0.1", time.Minute, "")
	RecordAuthFailure(rdb, "127.0.0.1", time.Minute, "")
	got, _ := rdb.Get(ctxBG(), "auth_fail:127.0.0.1").Int()
	if got != 2 {
		t.Fatalf("expected 2 recorded failures, got %d", got)
	}
}

// TestAuthFailureLimit_BlocksAtThreshold verifies an IP is blocked once the
// failure counter (set by RecordAuthFailure) reaches the threshold, and that the
// counter key gets a TTL.
func TestAuthFailureLimit_BlocksAtThreshold(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	defer mr.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthFailureLimit(rdb, 3, time.Minute, ""))
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Seed 3 failures (the threshold).
	for i := 0; i < 3; i++ {
		RecordAuthFailure(rdb, "10.0.0.9", time.Minute, "")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after %d failures, got %d", 3, w.Code)
	}
	ttl := mr.TTL("auth_fail:10.0.0.9")
	if ttl <= 0 {
		t.Fatalf("expected failure counter to have a TTL, got %v", ttl)
	}
}

// TestClearAuthFailures verifies the success path resets the counter.
func TestClearAuthFailures(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	defer mr.Close()

	RecordAuthFailure(rdb, "1.2.3.4", time.Minute, "")
	RecordAuthFailure(rdb, "1.2.3.4", time.Minute, "")
	ClearAuthFailures(rdb, "1.2.3.4", "")
	exists, _ := rdb.Exists(ctxBG(), "auth_fail:1.2.3.4").Result()
	if exists != 0 {
		t.Fatalf("expected counter cleared, still exists")
	}
}

// ── Audit item 10: Redis-independent fallback ──

// resetFallback clears the in-process fallback between tests (shared state).
func resetFallback() {
	authFailFallback.mu.Lock()
	authFailFallback.ips = map[string]*authFailBucket{}
	authFailFallback.mu.Unlock()
}

// With Redis DOWN, the per-process fallback must still throttle: recorded
// failures accumulate locally, the limiter blocks at the threshold, and a
// cleared IP is unblocked (no permanent lockout from stale local state).
func TestAuthFailureLimit_RedisDownFallsBackInProcess(t *testing.T) {
	resetFallback()
	rdb, mr := setupTestRedis(t)
	mr.Close() // kill Redis — every rdb call now errors

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthFailureLimit(rdb, 3, time.Minute, ""))
	r.Use(func(c *gin.Context) { c.Status(http.StatusOK) })

	get := func(ip string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = ip + ":1000"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := get("10.9.9.9"); code != http.StatusOK {
		t.Fatalf("clean IP should pass, got %d", code)
	}
	RecordAuthFailure(rdb, "10.9.9.9", time.Minute, "") // bumps fallback too
	RecordAuthFailure(rdb, "10.9.9.9", time.Minute, "")
	if code := get("10.9.9.9"); code != http.StatusOK {
		t.Fatalf("below threshold should pass, got %d", code)
	}
	RecordAuthFailure(rdb, "10.9.9.9", time.Minute, "")
	if code := get("10.9.9.9"); code != http.StatusTooManyRequests {
		t.Fatalf("threshold reached with Redis down must 429 via fallback, got %d", code)
	}
	ClearAuthFailures(rdb, "10.9.9.9", "")
	if code := get("10.9.9.9"); code != http.StatusOK {
		t.Fatalf("cleared IP must unblock (also in fallback), got %d", code)
	}
}

// Without any Redis configured, the fallback alone still throttles.
func TestAuthFailureLimit_NoRedisStillLimits(t *testing.T) {
	resetFallback()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthFailureLimit(nil, 2, time.Minute, ""))
	r.Use(func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 2; i++ {
		RecordAuthFailure(nil, "10.8.8.8", time.Minute, "")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.8.8.8:1000"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("no-Redis deployment must still throttle via fallback, got %d", w.Code)
	}
}

// The fallback window expires: stale buckets must not block forever.
func TestAuthFailureLimit_FallbackWindowExpires(t *testing.T) {
	resetFallback()
	authFailFallback.mu.Lock()
	authFailFallback.ips["10.7.7.7"] = &authFailBucket{count: 99, windowEnd: time.Now().Add(-time.Second)}
	authFailFallback.mu.Unlock()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthFailureLimit(nil, 3, time.Minute, ""))
	r.Use(func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.7.7.7:1000"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expired fallback bucket must not block, got %d", w.Code)
	}
}

// Audit item 10 (ordering): the limiter must run BEFORE the credential check —
// a blocked IP gets 429 without the Auth middleware ever executing.
func TestAuthFailureLimit_MountedBeforeAuth(t *testing.T) {
	resetFallback()
	rdb, mr := setupTestRedis(t)
	defer mr.Close()

	authReached := false
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Same order as gwGroup/usageGroup/ragGroup after the fix.
	r.Use(AuthFailureLimit(rdb, 3, time.Minute, ""))
	r.Use(func(c *gin.Context) {
		authReached = true // stands in for Auth: must NOT run for blocked IPs
		c.Status(http.StatusOK)
	})

	for i := 0; i < 3; i++ {
		RecordAuthFailure(rdb, "10.6.6.6", time.Minute, "")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.6.6.6:1000"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked IP must get 429, got %d", w.Code)
	}
	if authReached {
		t.Fatal("credential check must not execute for a blocked IP (limiter must precede Auth)")
	}
}

// ctxBG returns a background context for ad-hoc redis reads in tests.
func ctxBG() context.Context { return context.Background() }
