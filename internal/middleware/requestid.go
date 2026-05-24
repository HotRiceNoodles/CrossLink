package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id != "" {
			// Sanitize: max 64 chars, only alphanumeric/dash/underscore
			if len(id) > 64 {
				id = id[:64]
			}
			for _, r := range id {
				if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
					id = ""
					break
				}
			}
		}
		if id == "" {
			id = uuid.New().String()[:8]
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}
