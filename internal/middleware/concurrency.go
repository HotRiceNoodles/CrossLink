package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func ConcurrencyLimit(maxConcurrent int) gin.HandlerFunc {
	sem := make(chan struct{}, maxConcurrent)
	return func(c *gin.Context) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			c.Next()
		default:
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"type":  "error",
				"error": gin.H{"type": "rate_limit_error", "message": "server at capacity, try again"},
			})
			c.Abort()
		}
	}
}
