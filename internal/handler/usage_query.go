package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/service"
)

// UsageQueryHandler lets an API key holder query its own real-time quota/usage
// status (budget spent/remaining, call-count used/remaining, limits). It reads
// only the caller's own key from gin context — a key can never read another's data.
type UsageQueryHandler struct {
	budgetSvc service.BudgetServiceInterface
}

func NewUsageQueryHandler(budgetSvc service.BudgetServiceInterface) *UsageQueryHandler {
	return &UsageQueryHandler{budgetSvc: budgetSvc}
}

type usageAPIKey struct {
	ID        int64      `json:"id"`
	Status    int16      `json:"status"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type usageQuotaFloat struct {
	Period    string  `json:"period"`
	Limit     float64 `json:"limit"`
	Spent     float64 `json:"spent"`
	Remaining float64 `json:"remaining"`
	Exceeded  bool    `json:"exceeded"`
}

type usageQuotaCalls struct {
	Period    string `json:"period"`
	Limit     int    `json:"limit"`
	Used      int    `json:"used"`
	Remaining int    `json:"remaining"`
	Exceeded  bool   `json:"exceeded"`
}

type usageResponse struct {
	APIKey usageAPIKey       `json:"api_key"`
	Budget usageQuotaFloat   `json:"budget"`
	Calls  usageQuotaCalls   `json:"calls"`
	Limits map[string]int    `json:"limits"`
}

// GetUsage returns the caller's own real-time quota status.
// Requires middleware.Auth to have run; a post-Auth nil key (config-authkey
// fallback) yields 403.
func (h *UsageQueryHandler) GetUsage(c *gin.Context) {
	key := middleware.GetAPIKeyFromContext(c)
	if key == nil {
		c.JSON(http.StatusForbidden, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "permission_error",
				"message": "usage query requires a database-managed api key",
			},
		})
		return
	}

	if h.budgetSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "service_unavailable",
				"message": "budget service not enabled",
			},
		})
		return
	}

	ctx := c.Request.Context()
	keyID := fmt.Sprintf("%d", key.ID)

	// Read via CheckBudget/CheckCallLimit so the period→Redis-key derivation
	// stays delegated (matches the middleware's write path). Never hand-build keys.
	spent, budgetLimit, budgetExceeded := h.budgetSvc.CheckBudget(
		ctx, "key", keyID, key.BudgetPeriod, key.MaxBudget,
	)
	used, callExceeded := h.budgetSvc.CheckCallLimit(
		ctx, keyID, key.CallPeriod, key.MaxCalls,
	)

	resp := usageResponse{
		APIKey: usageAPIKey{ID: key.ID, Status: key.Status, ExpiresAt: key.ExpiresAt},
		Budget: usageQuotaFloat{
			Period: key.BudgetPeriod, Limit: budgetLimit, Spent: spent,
			Remaining: remainingFloat(budgetLimit, spent), Exceeded: budgetExceeded,
		},
		Calls: usageQuotaCalls{
			Period: key.CallPeriod, Limit: key.MaxCalls, Used: used,
			Remaining: remainingInt(key.MaxCalls, used), Exceeded: callExceeded,
		},
		Limits: map[string]int{
			"tpm_limit": key.TPMLimit,
			"rpm_limit": key.RPMLimit,
		},
	}
	c.JSON(http.StatusOK, resp)
}

func remainingFloat(limit, spent float64) float64 {
	if r := limit - spent; r > 0 {
		return r
	}
	return 0
}

func remainingInt(limit, used int) int {
	if r := limit - used; r > 0 {
		return r
	}
	return 0
}
