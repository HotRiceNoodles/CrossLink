package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runLogin performs one request through the login limiter with a handler that
// always fails login (non-200), returning the response status.
func runLogin(r *gin.Engine, ip string) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = ip + ":12345"
	r.ServeHTTP(w, req)
	return w.Code
}

func TestLoginRateLimit_Boundary(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(LoginRateLimit(rdb, 3, time.Minute))
	r.POST("/login", func(c *gin.Context) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "bad credentials"})
	})

	// Attempts 1..max are allowed (each fails login, counter increments).
	for i := 1; i <= 3; i++ {
		assert.Equal(t, http.StatusUnauthorized, runLogin(r, "10.0.0.1"),
			"attempt %d within limit must reach the handler", i)
	}
	// Attempts beyond the limit are rejected — regression guard for the
	// sentinel bug where the Lua script returned exactly max and the strict
	// > comparison let every subsequent request through.
	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusTooManyRequests, runLogin(r, "10.0.0.1"),
			"attempt beyond limit must be rejected")
	}
}

func TestLoginRateLimit_SuccessResetsCounter(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(LoginRateLimit(rdb, 2, time.Minute))
	okHandler := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"token": "t"}) }
	r.POST("/login-ok", okHandler)
	r.POST("/login-fail", func(c *gin.Context) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "bad credentials"})
	})

	doRequest := func(path string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "10.0.0.2:12345"
		r.ServeHTTP(w, req)
		return w.Code
	}

	assert.Equal(t, http.StatusUnauthorized, doRequest("/login-fail"))
	assert.Equal(t, http.StatusOK, doRequest("/login-ok")) // success clears the key
	assert.Equal(t, http.StatusUnauthorized, doRequest("/login-fail")) // attempt 1 after reset
	assert.Equal(t, http.StatusUnauthorized, doRequest("/login-fail")) // attempt 2 = max
	assert.Equal(t, http.StatusTooManyRequests, doRequest("/login-fail"))
}

func TestLoginRateLimit_PerIP(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(LoginRateLimit(rdb, 1, time.Minute))
	r.POST("/login", func(c *gin.Context) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "bad credentials"})
	})

	assert.Equal(t, http.StatusUnauthorized, runLogin(r, "10.0.0.3"))
	assert.Equal(t, http.StatusTooManyRequests, runLogin(r, "10.0.0.3"))
	assert.Equal(t, http.StatusUnauthorized, runLogin(r, "10.0.0.4"))
}
