package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/service"
)

// defaultBudgetOutputTokens is the output-cost estimate used when a request does
// not declare max_tokens. It trades off false rejects near the limit (high value)
// against concurrency-overspend precision (low value). 1000 is a middle ground;
// requests that declare max_tokens reserve their actual cap.
const defaultBudgetOutputTokens = 1000

// reserveBudgetForRequest atomically reserves an estimated cost against the
// budget scopes stashed in context by the BudgetCheck middleware (key/team/org),
// closing the concurrent check-then-act race that GET-based budget checks cannot.
// The estimate uses the primary route's prices: (input·inPrice + maxOutput·outPrice)/1000.
//
// Returns true if the request may proceed (reserved OK, or no budget/estimate).
// Returns false (and writes a budget_exceeded response + aborts) if a level would
// be exceeded. The reservation is stashed in context as "budget_reservations" so
// ReportBudgetUsage can reconcile each counter by (actual - reserved).
func reserveBudgetForRequest(c *gin.Context, budgetSvc *service.BudgetService, inputTokens, maxOutputTokens int, inPrice, outPrice float64) bool {
	if budgetSvc == nil {
		return true
	}
	scopesVal, _ := c.Get("budget_scopes")
	scopes, _ := scopesVal.([]service.BudgetScope)
	if len(scopes) == 0 {
		return true // no budget limits configured → nothing to reserve
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultBudgetOutputTokens
	}
	estimate := (float64(inputTokens)*inPrice + float64(maxOutputTokens)*outPrice) / 1000
	if estimate <= 0 {
		// Prices unknown/zero (e.g. unmetered model): can't estimate, so skip
		// reservation. Actual cost is still reported by ReportBudgetUsage.
		return true
	}
	res, exceededScope := budgetSvc.ReserveForRequest(c.Request.Context(), scopes, estimate)
	if exceededScope != "" {
		// A higher level rejected: lower levels were already refunded inside
		// ReserveForRequest. Do NOT stash reservations — the request is aborted
		// before the upstream call, so ReportBudgetUsage must not reconcile again.
		c.Set("budget_exceeded", true)
		c.JSON(http.StatusTooManyRequests, gin.H{
			"type":  "error",
			"error": gin.H{"type": "budget_exceeded", "message": fmt.Sprintf("%s budget would be exceeded by this request", exceededScope)},
		})
		c.Abort()
		return false
	}
	c.Set("budget_reservations", res)
	return true
}
