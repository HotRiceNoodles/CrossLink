package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// KeyValidator validates raw API keys against the database.
type KeyValidator interface {
	Validate(ctx context.Context, rawKey string) (*model.APIKey, error)
}

// ErrKeyExpired is returned when the API key has expired.
var errKeyExpired = errors.New("api key expired")

// MCPAuth authenticates MCP-surface requests. windowPolicy mirrors the
// gateway Auth seam (design: docs/design/api-key-time-window-control.md §5):
// a time-window-restricted key must be denied here too — deny is deny, the
// debug/MCP surfaces do not widen admission. nil policy = no restriction.
func MCPAuth(configAuthKey string, keySvc KeyValidator, windowPolicy service.KeyWindowPolicy) gin.HandlerFunc {
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
				if windowPolicy != nil {
					if werr := windowPolicy.Check(key, time.Now()); werr != nil {
						var twe *service.TimeWindowError
						if !errors.As(werr, &twe) {
							twe = &service.TimeWindowError{Invalid: true}
						}
						if twe.Invalid {
							middleware.RecordTimeWindowDenial("invalid")
							c.JSON(http.StatusForbidden, gin.H{"error": "api key time-window policy is invalid; contact an administrator", "error_code": "time_window_invalid"})
							c.Abort()
							return
						}
						middleware.RecordTimeWindowDenial("denied")
						msg := "API key is outside its allowed time windows."
						if !twe.NextOpen.IsZero() {
							msg = fmt.Sprintf("API key is outside its allowed time windows. Next open at %s.", middleware.FormatWallClock(twe.NextOpen, twe.TZ))
							if wait := time.Until(twe.NextOpen); wait > 0 {
								if wait > 24*time.Hour {
									wait = 24 * time.Hour
								}
								c.Header("Retry-After", fmt.Sprintf("%d", int(wait.Seconds()+0.5)))
							}
						}
						c.JSON(http.StatusForbidden, gin.H{"error": msg, "error_code": "time_window_denied"})
						c.Abort()
						return
					}
				}
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
