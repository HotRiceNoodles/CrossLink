package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestID mints the INTERNAL billing/audit request id and separates it
// from any client-supplied X-Request-ID (billing-collision fix, 2026-10-21
// review): the budget markers (resvmark/settledmark), the settle ledger and
// the usage rows all key idempotency on c.GetString("request_id"), so that
// value MUST be (a) server-minted with full entropy — a full UUID v4
// (122 random bits; the previous uuid[:8] mint collided ~once per ~93k
// requests, 41 observed in r59's 607,854) and (b) NEVER
// client-influenced — a client replaying one X-Request-ID used to share
// markers across requests, skipping reserves and no-op'ing settles
// (systematic Redis-billing bypass). The client header is preserved under
// "client_request_id" for tracing/correlation only, and echoed back in the
// response so clients can correlate.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		internal := uuid.New().String()
		c.Set("request_id", internal)

		client := c.GetHeader("X-Request-ID")
		if client != "" {
			// Sanitize for log safety: max 64 chars, alphanumeric/dash/underscore.
			if len(client) > 64 {
				client = client[:64]
			}
			for _, r := range client {
				if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
					client = ""
					break
				}
			}
			if client != "" {
				c.Set("client_request_id", client)
			}
		}

		if client != "" {
			c.Header("X-Request-ID", client)
		} else {
			c.Header("X-Request-ID", internal)
		}
		c.Next()
	}
}
