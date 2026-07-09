package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/service"
)

func ReportBudgetUsage(budgetSvc service.BudgetServiceInterface, alertSvc service.BudgetAlertServiceInterface, teamCache *TeamCache, orgCache *OrgCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if budgetSvc == nil {
			return
		}

		apiKey := GetAPIKeyFromContext(c)
		if apiKey == nil {
			return
		}

		inTok, _ := c.Get("input_tokens")
		outTok, _ := c.Get("output_tokens")
		inPrice, _ := c.Get("input_price")
		outPrice, _ := c.Get("output_price")

		_, hasPrecomputedCost := c.Get("cost")

		// Report call count usage — independent of cost calculation
		if apiKey.MaxCalls > 0 {
			callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
			budgetSvc.ReportCallUsage(callCtx, fmt.Sprintf("%d", apiKey.ID), apiKey.CallPeriod)
			callCancel()
		}

		if inTok == nil && outTok == nil && !hasPrecomputedCost {
			return
		}

		inputTokens, _ := inTok.(int)
		outputTokens, _ := outTok.(int)
		inputPrice, _ := inPrice.(float64)
		outputPrice, _ := outPrice.(float64)

		var cost float64
		if precomputedCost, exists := c.Get("cost"); exists {
			cost = precomputedCost.(float64)
		} else {
			cost = inputPrice*float64(inputTokens)/1000 + outputPrice*float64(outputTokens)/1000
		}

		// C5: if a pre-request reservation was made, reconcile each counter by
		// (actual - reserved) instead of adding the full actual cost on top. This
		// also refunds the reservation when cost == 0 (failed/zero-token request),
		// so reservations never leak as phantom spend.
		resVal, _ := c.Get("budget_reservations")
		reservations, _ := resVal.(*service.BudgetReservations)

		if cost <= 0 && (reservations == nil || len(reservations.Reserved) == 0) {
			return
		}

		// Use background context for all post-response Redis calls;
		// c.Request.Context() may be cancelled after response is written.
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()

		// Report key-level budget usage
		if apiKey.MaxBudget > 0 {
			applyBudgetDelta(budgetSvc, bgCtx, "key",
				fmt.Sprintf("%d", apiKey.ID), apiKey.BudgetPeriod, cost, reservations)

			// Check key-level alerts (async, own context)
			spent, limit, _ := budgetSvc.CheckBudget(bgCtx, "key",
				fmt.Sprintf("%d", apiKey.ID), apiKey.BudgetPeriod, apiKey.MaxBudget)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("budget alert goroutine panic", "error", r)
					}
				}()
				alertCtx, alertCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer alertCancel()
				alertSvc.CheckAndAlert(alertCtx, "key",
					fmt.Sprintf("%d", apiKey.ID), apiKey.BudgetPeriod, spent, limit)
			}()
		}

		// Report team-level budget usage
		if teamPeriod, ok := c.Get("team_budget_period"); ok {
			if p, _ := teamPeriod.(string); p != "" && apiKey.TeamID != nil {
				applyBudgetDelta(budgetSvc, bgCtx, "team",
					fmt.Sprintf("%d", *apiKey.TeamID), p, cost, reservations)

				// Check team-level alerts (async, own context)
				team := teamCache.Get(bgCtx, *apiKey.TeamID)
				if team != nil {
					spent, limit, _ := budgetSvc.CheckBudget(bgCtx, "team",
						fmt.Sprintf("%d", team.ID), team.BudgetPeriod, team.BudgetLimit)
					go func() {
						defer func() {
							if r := recover(); r != nil {
								slog.Warn("budget alert goroutine panic", "error", r)
							}
						}()
						alertCtx, alertCancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer alertCancel()
						alertSvc.CheckAndAlert(alertCtx, "team",
							fmt.Sprintf("%d", team.ID), team.BudgetPeriod, spent, limit)
					}()
				}
			}
		}

		// 3. Org-level reporting
		if orgCache != nil && cost > 0 {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				orgPd, _ := c.Get("org_budget_period")
				if orgPeriod, ok := orgPd.(string); ok && orgPeriod != "" {
					applyBudgetDelta(budgetSvc, bgCtx, "org", fmt.Sprintf("%d", orgID), orgPeriod, cost, reservations)
					go func() {
						defer func() { recover() }()
						org := orgCache.Get(context.Background(), orgID)
						if org != nil && org.BudgetLimit > 0 {
							alertSvc.CheckAndAlert(context.Background(), "org", fmt.Sprintf("%d", orgID), orgPeriod, cost, org.BudgetLimit)
						}
					}()
				}
			}
		}
	}
}

// applyBudgetDelta reconciles one budget counter. If a reservation was made for
// this scope, adjust by (cost - reserved); otherwise report the full cost (the
// legacy path for requests that did not reserve, e.g. estimate was zero).
func applyBudgetDelta(budgetSvc service.BudgetServiceInterface, ctx context.Context, scope, targetID, period string, cost float64, reservations *service.BudgetReservations) {
	if reservations != nil {
		if reserved, ok := reservations.Reserved[scope]; ok {
			budgetSvc.AdjustBudget(ctx, scope, targetID, period, cost-reserved)
			return
		}
	}
	budgetSvc.ReportUsage(ctx, scope, targetID, period, cost)
}

// AdminReportBudgetUsage reports team and org budget usage for admin/playground routes.
// It reads team_id/org_id from JWT context instead of api_key.
// Pass nil budgetSvc to disable (Community mode).
func AdminReportBudgetUsage(budgetSvc service.BudgetServiceInterface, alertSvc service.BudgetAlertServiceInterface, teamCache *TeamCache, orgCache *OrgCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if budgetSvc == nil {
			return
		}

		teamIDVal, _ := c.Get("team_id")
		tid, ok := teamIDVal.(int64)
		if !ok || tid <= 0 {
			return
		}

		inTok, _ := c.Get("input_tokens")
		outTok, _ := c.Get("output_tokens")
		inPrice, _ := c.Get("input_price")
		outPrice, _ := c.Get("output_price")

		inputTokens, _ := inTok.(int)
		outputTokens, _ := outTok.(int)
		inputPrice, _ := inPrice.(float64)
		outputPrice, _ := outPrice.(float64)

		var cost float64
		if precomputedCost, exists := c.Get("cost"); exists {
			cost = precomputedCost.(float64)
		} else {
			cost = inputPrice*float64(inputTokens)/1000 + outputPrice*float64(outputTokens)/1000
		}
		if cost <= 0 {
			return
		}

		bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()

		team := teamCache.Get(bgCtx, tid)
		if team != nil && team.BudgetLimit > 0 {
			teamPeriod, _ := c.Get("team_budget_period")
			p, _ := teamPeriod.(string)
			if p == "" {
				p = team.BudgetPeriod
			}

			budgetSvc.ReportUsage(bgCtx, "team",
				fmt.Sprintf("%d", team.ID), p, cost)

			spent, limit, _ := budgetSvc.CheckBudget(bgCtx, "team",
				fmt.Sprintf("%d", team.ID), team.BudgetPeriod, team.BudgetLimit)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("admin budget alert goroutine panic", "error", r)
					}
				}()
				alertCtx, alertCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer alertCancel()
				alertSvc.CheckAndAlert(alertCtx, "team",
					fmt.Sprintf("%d", team.ID), team.BudgetPeriod, spent, limit)
			}()
		}

		// Org-level reporting
		if orgCache != nil && cost > 0 {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				orgPd, _ := c.Get("org_budget_period")
				if orgPeriod, ok := orgPd.(string); ok && orgPeriod != "" {
					budgetSvc.ReportUsage(bgCtx, "org", fmt.Sprintf("%d", orgID), orgPeriod, cost)
					go func() {
						defer func() { recover() }()
						org := orgCache.Get(context.Background(), orgID)
						if org != nil && org.BudgetLimit > 0 {
							alertSvc.CheckAndAlert(context.Background(), "org", fmt.Sprintf("%d", orgID), orgPeriod, cost, org.BudgetLimit)
						}
					}()
				}
			}
		}
	}
}
