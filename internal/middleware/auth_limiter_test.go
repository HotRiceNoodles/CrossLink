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

// ctxBG returns a background context for ad-hoc redis reads in tests.
func ctxBG() context.Context { return context.Background() }
