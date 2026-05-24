package middleware

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
)

// NOTE: This middleware does not rate-limit failed authentication attempts on the gateway API.
// Admin login paths are protected by LoginRateLimit middleware. For gateway API key brute-force
// protection, deploy a WAF or reverse proxy rate limiter in front of the gateway.
func Auth(authKey string, keySvc *service.KeyService, rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := extractAPIKey(c)
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"type":  "error",
				"error": gin.H{"type": "authentication_error", "message": "x-api-key header is required"},
			})
			c.Abort()
			return
		}

		// Try database key validation
		if keySvc != nil {
			key, err := keySvc.Validate(c.Request.Context(), apiKey)
			if err == nil && key != nil {
				c.Set("api_key", key)
				c.Set("api_key_id", key.ID)
				ClearAuthFailures(rdb, c.ClientIP(), "")
				c.Next()
				return
			}
			if errors.Is(err, service.ErrKeyExpired) {
				c.JSON(http.StatusForbidden, gin.H{
					"type":  "error",
					"error": gin.H{"type": "permission_error", "message": "api key has expired"},
				})
				c.Abort()
				return
			}
		}

		// Fallback to config auth key
		// NOTE: The config auth key has no expiration or disable mechanism.
		// If compromised, restart the server with a new config.
		if authKey != "" && subtle.ConstantTimeCompare([]byte(apiKey), []byte(authKey)) == 1 {
			ClearAuthFailures(rdb, c.ClientIP(), "")
			c.Next()
			return
		}

		RecordAuthFailure(rdb, c.ClientIP(), 20, 15*time.Minute, "")
		c.JSON(http.StatusUnauthorized, gin.H{
			"type":  "error",
			"error": gin.H{"type": "authentication_error", "message": "invalid api key"},
		})
		c.Abort()
	}
}

// RequireModel checks if the API key allows the requested model.
// Must be placed after Auth middleware. Reads model from request body
// without consuming it by using GetRawData + re-setting it.
func RequireModel() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := GetAPIKeyFromContext(c)
		if key == nil {
			c.Next()
			return
		}

		if len(key.AllowedModels) == 0 {
			c.Next()
			return
		}

		var models []string
		if err := json.Unmarshal(key.AllowedModels, &models); err != nil {
			c.Next()
			return
		}

		if len(models) == 0 {
			c.Next()
			return
		}

		// Multipart requests cannot be JSON-parsed for model extraction.
		// Skip body read entirely to avoid truncating large file uploads.
		if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
			c.Next()
			return
		}

		// Read body — prefer cached bytes from ReadBody middleware
		bodyBytes := GetBodyBytes(c)
		if bodyBytes == nil {
			var err error
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
			if err != nil {
				c.Next()
				return
			}
			c.Request.Body.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		var body struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(bodyBytes, &body) != nil || body.Model == "" {
			c.Next()
			return
		}

		allowed := false
		for _, m := range models {
			if m == body.Model {
				allowed = true
				break
			}
		}

		if !allowed {
			c.JSON(http.StatusForbidden, gin.H{
				"type":  "error",
				"error": gin.H{"type": "permission_error", "message": "model not allowed for this key"},
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// RequireRoute checks if the API key allows the requested route type.
// routeType should be "anthropic" or "openai".
func RequireRoute(routeType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := GetAPIKeyFromContext(c)
		if key == nil {
			c.Next()
			return
		}

		if len(key.AllowedRoutes) == 0 {
			c.Next()
			return
		}

		var routes []string
		if err := json.Unmarshal(key.AllowedRoutes, &routes); err != nil {
			c.Next()
			return
		}

		if len(routes) == 0 {
			c.Next()
			return
		}

		allowed := false
		for _, r := range routes {
			if r == routeType {
				allowed = true
				break
			}
		}

		if !allowed {
			c.JSON(http.StatusForbidden, gin.H{
				"type":  "error",
				"error": gin.H{"type": "permission_error", "message": "route not allowed for this key"},
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

func extractAPIKey(c *gin.Context) string {
	apiKey := c.GetHeader("x-api-key")
	if apiKey == "" {
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return apiKey
}

// ExtractAPIKey extracts the API key from x-api-key header or Authorization Bearer token.
func ExtractAPIKey(c *gin.Context) string {
	return extractAPIKey(c)
}

// GetAPIKeyFromContext retrieves the validated API key from gin context.
func GetAPIKeyFromContext(c *gin.Context) *model.APIKey {
	val, exists := c.Get("api_key")
	if !exists {
		return nil
	}
	key, ok := val.(*model.APIKey)
	if !ok {
		return nil
	}
	return key
}
