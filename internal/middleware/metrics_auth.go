package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

// GatewayMetricsAuth protects the /metrics endpoint with the gateway auth key.
func GatewayMetricsAuth(authKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("Authorization")
		if len(key) > 7 && key[:7] == "Bearer " {
			key = key[7:]
		}
		if key == "" {
			key = c.GetHeader("x-api-key")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(authKey)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
