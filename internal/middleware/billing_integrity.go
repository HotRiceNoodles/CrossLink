package middleware

import (
	"net/http"

	"github.com/crosslink/internal/billingintegrity"
	"github.com/gin-gonic/gin"
)

// BillingIntegrityGate rejects NEW billable requests while the billing
// ledger is unpersistable (DB+WAL double failure, policy
// usage.loss_policy=block). In-flight responses are untouched — their
// durability was determined at completion; the block protects future rows.
func BillingIntegrityGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !billingintegrity.Blocked() {
			c.Next()
			return
		}
		_, since, reason := billingintegrity.BlockInfo()
		c.Header("Retry-After", "5")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "billing_integrity_blocked",
				"message": "usage ledger persistence is degraded; new billable requests are temporarily rejected",
				"reason":  reason,
				"since":   since,
			},
		})
		c.Abort()
	}
}
