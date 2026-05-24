package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
)

// KeyValidator validates raw API keys against the database.
type KeyValidator interface {
	Validate(ctx context.Context, rawKey string) (*model.APIKey, error)
}

// ErrKeyExpired is returned when the API key has expired.
var errKeyExpired = errors.New("api key expired")

func MCPAuth(configAuthKey string, keySvc KeyValidator) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := middleware.ExtractAPIKey(c)
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing API key"})
			c.Abort()
			return
		}

		// Try database key validation first
		if keySvc != nil {
			key, err := keySvc.Validate(c.Request.Context(), apiKey)
			if err == nil && key != nil {
				c.Set("api_key", key)
				c.Set("api_key_id", key.ID)
				c.Set("auth_via", "database")
				c.Next()
				return
			}
			if errors.Is(err, errKeyExpired) {
				c.JSON(http.StatusForbidden, gin.H{"error": "api key has expired"})
				c.Abort()
				return
			}
		}

		// Fallback to config auth key
		if configAuthKey != "" && subtle.ConstantTimeCompare([]byte(apiKey), []byte(configAuthKey)) == 1 {
			c.Set("auth_via", "config")
			c.Next()
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
		c.Abort()
	}
}

func MCPRateLimit(limiter *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.ClientIP()
		if keyID, exists := c.Get("api_key_id"); exists {
			if id, ok := keyID.(int64); ok {
				key = fmt.Sprintf("key:%d", id)
			}
		}
		if !limiter.Allow(key) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			c.Abort()
			return
		}
		c.Next()
	}
}
