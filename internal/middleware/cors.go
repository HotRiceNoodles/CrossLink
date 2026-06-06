package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORSConfig holds allowed origins for the CORS middleware.
type CORSConfig struct {
	AllowedOrigins []string
}

// DefaultCORSConfig returns a safe default (localhost only).
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		AllowedOrigins: []string{
			"http://localhost:5173",
			"http://localhost:3000",
			"http://localhost:8080",
			"http://127.0.0.1:5173",
			"http://127.0.0.1:3000",
			"http://127.0.0.1:8080",
		},
	}
}

// CORS returns a middleware that validates the Origin header against an allowlist.
func CORS(cfg ...CORSConfig) gin.HandlerFunc {
	var allowed map[string]bool
	if len(cfg) > 0 && len(cfg[0].AllowedOrigins) > 0 {
		allowed = make(map[string]bool, len(cfg[0].AllowedOrigins))
		for _, o := range cfg[0].AllowedOrigins {
			allowed[strings.ToLower(o)] = true
		}
	} else {
		c := DefaultCORSConfig()
		allowed = make(map[string]bool, len(c.AllowedOrigins))
		for _, o := range c.AllowedOrigins {
			allowed[strings.ToLower(o)] = true
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && allowed[strings.ToLower(origin)] {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, X-Requested-With")
		c.Header("Vary", "Origin")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// CSRFGuard rejects state-changing requests (POST, PUT, DELETE, PATCH) to admin API
// that lack the X-Requested-With header. HTML forms cannot set custom headers,
// so this prevents cross-origin form-submission CSRF attacks.
func CSRFGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		if method == "POST" || method == "PUT" || method == "DELETE" || method == "PATCH" {
			path := c.Request.URL.Path
			// Skip gateway API paths (authenticated via API key, not cookies)
			if len(path) >= 3 && path[:3] == "/v1" {
				c.Next()
				return
			}
			// Skip login/logout (no JWT yet)
			if strings.HasSuffix(path, "/auth/login") || strings.HasSuffix(path, "/auth/logout") {
				c.Next()
				return
			}
			xrw := c.GetHeader("X-Requested-With")
			if xrw == "" {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "missing X-Requested-With header"})
				return
			}
		}
		c.Next()
	}
}
