package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/service"
)

// PortalUsageHandler serves the customer self-service portal. Unlike
// UsageQueryHandler (which exposes the raw /v1/usage contract used by SDKs),
// this handler returns a CLEAN portal-owned contract: booleans for active /
// unlimited, no raw status int, no limit==0 sentinel, no snake_case internals.
//
// The portal frontend binds only to this contract, so the internal /v1/usage
// shape can evolve freely without forcing portal frontend changes. This is the
// intentional decoupling boundary between the gateway and the portal UI.
type PortalUsageHandler struct {
	budgetSvc service.BudgetServiceInterface
}

func NewPortalUsageHandler(budgetSvc service.BudgetServiceInterface) *PortalUsageHandler {
	return &PortalUsageHandler{budgetSvc: budgetSvc}
}

// portalKey is the key-holder view: only whether it is active and when it expires.
// Raw status int / id / name / prefix are intentionally NOT exposed.
type portalKey struct {
	Active    bool    `json:"active"`
	ExpiresAt *string `json:"expires_at"` // RFC3339, or nil when not set
}

// portalMetric is a generic limit/current/remaining triple used for both budget
// (money) and calls (count). `Unlimited` is explicit so consumers never have to
// infer it from limit==0.
type portalMetric struct {
	Period    string  `json:"period"`
	Limit     float64 `json:"limit"`
	Current   float64 `json:"current"`
	Remaining float64 `json:"remaining"`
	Unlimited bool    `json:"unlimited"`
	Exceeded  bool    `json:"exceeded"`
}

// portalLimits surfaces TPM/RPM with explicit unlimited flags.
type portalLimits struct {
	TPM          int  `json:"tpm"`
	RPM          int  `json:"rpm"`
	TPMUnlimited bool `json:"tpm_unlimited"`
	RPMUnlimited bool `json:"rpm_unlimited"`
}

type portalUsageResponse struct {
	Key    portalKey    `json:"key"`
	Budget portalMetric `json:"budget"`
	Calls  portalMetric `json:"calls"`
	Limits portalLimits `json:"limits"`
}

// activeStatus is the APIKey.Status value meaning "active". Resolving it here
// (instead of leaking the int to the frontend) is the core of the decoupling.
const activeStatus int16 = 1

// GetUsage returns the caller's own quota in the portal contract.
// Requires middleware.Auth; a post-Auth nil key (config-authkey fallback) → 403.
func (h *PortalUsageHandler) GetUsage(c *gin.Context) {
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

	spent, _, budgetExceeded := h.budgetSvc.CheckBudget(ctx, "key", keyID, key.BudgetPeriod, key.MaxBudget)
	used, callExceeded := h.budgetSvc.CheckCallLimit(ctx, keyID, key.CallPeriod, key.MaxCalls)

	c.JSON(http.StatusOK, portalUsageResponse{
		Key: portalKey{
			Active:    key.Status == activeStatus,
			ExpiresAt: formatExpiresAt(key.ExpiresAt),
		},
		Budget: portalMetric{
			Period: key.BudgetPeriod, Limit: key.MaxBudget, Current: spent,
			Remaining: remainingFloat(key.MaxBudget, spent),
			Unlimited: key.MaxBudget == 0, Exceeded: budgetExceeded,
		},
		Calls: portalMetric{
			Period: key.CallPeriod, Limit: float64(key.MaxCalls), Current: float64(used),
			Remaining: float64(remainingInt(key.MaxCalls, used)),
			Unlimited: key.MaxCalls == 0, Exceeded: callExceeded,
		},
		Limits: portalLimits{
			TPM: key.TPMLimit, RPM: key.RPMLimit,
			TPMUnlimited: key.TPMLimit == 0, RPMUnlimited: key.RPMLimit == 0,
		},
	})
}

func formatExpiresAt(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
