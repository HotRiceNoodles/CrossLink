package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

func ReportBudgetUsage(budgetSvc service.BudgetServiceInterface, alertSvc service.BudgetAlertServiceInterface, teamCache *TeamCache, orgCache *OrgCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Deferred so reservations are reconciled (refunded) even when a
		// handler panics — otherwise the reservation leaks as phantom spend
		// until the hourly calibration fixes the counter.
		defer func() {
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

			// F1′ reservations outstanding (admission reserved e per scope).
			// Read BEFORE the early-exits: a request that dies before any
			// token accounting (upstream error, client abort mid-response)
			// still holds reservations that MUST be released — the r33c
			// "never-settled cohort" was exactly this early-return skipping
			// SettleScope while the usage pipeline wrote the ledger row.
			resVal, _ := c.Get("budget_reservations")
			reservations, _ := resVal.(*service.BudgetReservations)
			hasReservations := reservations != nil && len(reservations.Reserved) > 0

			// Call-count reconciliation. Calls are counted atomically at
			// admission (ReserveCall in BudgetCheck); here we only refund
			// paths that must not consume the limit: video status polling,
			// idempotent replays, and requests rejected before any upstream
			// work (skip_call_count). Keys that somehow arrive without an
			// admission reservation fall back to the legacy post-completion
			// count.
			_, skip := c.Get("skip_call_count")
			if reserved, _ := c.Get("call_reserved"); reserved == true {
				if skip {
					if pkv, ok := c.Get("call_reserved_pk"); ok {
						if pk, _ := pkv.(string); pk != "" {
							callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
							budgetSvc.RefundCall(callCtx, fmt.Sprintf("%d", apiKey.ID), pk)
							callCancel()
						}
					}
				}
			} else if !skip && apiKey.MaxCalls > 0 {
				callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
				budgetSvc.ReportCallUsage(callCtx, fmt.Sprintf("%d", apiKey.ID), apiKey.CallPeriod)
				callCancel()
			}

			if inTok == nil && outTok == nil && !hasPrecomputedCost && !hasReservations {
				// Debug-only: this early-return skips ALL budget settlement for
				// the request while the usage ledger row is written by the
				// separate usage pipeline — a systematic divergence source when
				// it fires unexpectedly (r18s exit-wave forensics). With
				// reservations outstanding it must NOT fire: the reservation
				// release below settles cost=0 (refund) against the marks.
				slog.Debug("budget settle skipped: no token usage on context",
					"request_id", c.GetString("request_id"), "path", c.FullPath())
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

			if cost <= 0 && !hasReservations {
				slog.Debug("budget settle skipped: zero cost and no reservations to refund",
					"request_id", c.GetString("request_id"))
				return
			}

			// Use background context for all post-response Redis calls;
			// c.Request.Context() may be cancelled after response is written.
			bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer bgCancel()

			// Report key-level budget usage
			if apiKey.MaxBudget > 0 {
				applyBudgetDelta(budgetSvc, bgCtx, "key",
					fmt.Sprintf("%d", apiKey.ID), apiKey.BudgetPeriod, cost, reservations, c.GetString("request_id"))

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
						fmt.Sprintf("%d", *apiKey.TeamID), p, cost, reservations, c.GetString("request_id"))

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

			// 3. Org-level reporting. No cost>0 gate: a failed request (cost=0)
			// must still run applyBudgetDelta so its reservation is refunded,
			// matching the key/team scopes above.
			if orgCache != nil {
				if orgID := c.GetInt64("org_id"); orgID != 0 {
					orgPd, _ := c.Get("org_budget_period")
					if orgPeriod, ok := orgPd.(string); ok && orgPeriod != "" {
						applyBudgetDelta(budgetSvc, bgCtx, "org", fmt.Sprintf("%d", orgID), orgPeriod, cost, reservations, c.GetString("request_id"))
						go func() {
							defer func() { recover() }()
							// Bounded ctx (matches the key/team alert branches):
							// an un-timed Background goroutine piles up when the
							// DB or alert path hangs.
							orgCtx, orgCancel := context.WithTimeout(context.Background(), 30*time.Second)
							defer orgCancel()
							org := orgCache.Get(orgCtx, orgID)
							if org != nil && org.BudgetLimit > 0 {
								alertSvc.CheckAndAlert(orgCtx, "org", fmt.Sprintf("%d", orgID), orgPeriod, cost, org.BudgetLimit)
							}
						}()
					}
				}
			}
		}()
		c.Next()
	}
}

// applyBudgetDelta reconciles one budget scope for a request. With a request
// id (F1′) settlement goes through SettleScope: exactly-once per
// (scope, request), reconciling against the per-request reservation MARK as
// ground truth — a landed reservation settles (cost-reserved), an un-landed
// one settles full cost; no reliance on in-memory bookkeeping. The legacy
// path (no request id, or no reservation recorded) reports the full cost.
func applyBudgetDelta(budgetSvc service.BudgetServiceInterface, ctx context.Context, scope, targetID, period string, cost float64, reservations *service.BudgetReservations, reqID string) {
	pk := ""
	if reservations != nil {
		pk = reservations.PeriodKeys[scope]
	}
	if reqID != "" {
		if ss, ok := budgetSvc.(interface {
			SettleScope(ctx context.Context, scope, targetID, period, periodKey, reqID string, cost float64)
		}); ok {
			ss.SettleScope(ctx, scope, targetID, period, pk, reqID, cost)
			return
		}
	}
	if reservations != nil {
		if reserved, ok := reservations.Reserved[scope]; ok {
			budgetSvc.AdjustBudgetKeyed(ctx, scope, targetID, period, reservations.PeriodKeys[scope], cost-reserved)
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
		tid, _ := teamIDVal.(int64)

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

		// Team reporting is independent of org reporting: a teamless org member
		// must still consume the org budget.
		if tid > 0 {
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
		}

		// Org-level reporting
		if orgCache != nil && cost > 0 {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				orgPd, _ := c.Get("org_budget_period")
				if orgPeriod, ok := orgPd.(string); ok && orgPeriod != "" {
					budgetSvc.ReportUsage(bgCtx, "org", fmt.Sprintf("%d", orgID), orgPeriod, cost)
					go func() {
						defer func() { recover() }()
						// Bounded ctx (matches the key/team alert branches).
						orgCtx, orgCancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer orgCancel()
						org := orgCache.Get(orgCtx, orgID)
						if org != nil && org.BudgetLimit > 0 {
							alertSvc.CheckAndAlert(orgCtx, "org", fmt.Sprintf("%d", orgID), orgPeriod, cost, org.BudgetLimit)
						}
					}()
				}
			}
		}
	}
}
