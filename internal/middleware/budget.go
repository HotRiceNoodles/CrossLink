package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// budgetScopeDegradeTotal counts requests whose TEAM/ORG budget scope was
// silently dropped because the metadata cache lookup failed (the r61
// finding: during a DB outage teamCache/orgCache miss -> the scope never
// enters `scopes` -> NO reserve AND NO settle for team/org — the request is
// enforced and settled at key scope only; 53/75,391 requests in a 120s
// PG-stop window, team/org counters healed by the hourly DB-based raise but
// the team/org LIMIT was not enforced for that cohort). Fail-open is the
// standing availability posture; this counter + sampled WARN make the
// degradation visible instead of silent.
var budgetScopeDegradeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cl_budget_scope_degrade_total",
	Help: "Requests whose team/org budget scope was dropped (metadata lookup failed, typically a DB outage): key-scope-only enforcement/settle until the hourly calibration raise.",
}, []string{"scope", "mode"})

var budgetScopeDegradeLogged atomic.Int64

func noteBudgetScopeDegrade(scope string, id int64) {
	budgetScopeDegradeTotal.WithLabelValues(scope, "rejected_cold").Inc()
	if budgetScopeDegradeLogged.Add(1)%100 == 1 {
		slog.Warn("budget scope DEGRADED: team/org metadata lookup failed (db outage window?) — key-scope-only enforcement/settle for this cohort until the next calibration raise",
			"scope", scope, "id", id)
	}
}

func BudgetCheck(budgetSvc service.BudgetServiceInterface, teamCache *TeamCache, orgCache *OrgCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		if budgetSvc == nil {
			c.Next()
			return
		}

		apiKey := GetAPIKeyFromContext(c)
		if apiKey == nil {
			c.Next()
			return
		}

		// Collect the budget scopes that have a limit, so the handler can do an
		// atomic pre-request reservation (closing the check-then-act race that the
		// GET-based checks above cannot). Order matters: key → team → org.
		scopes := make([]service.BudgetScope, 0, 3)

		if apiKey.MaxBudget > 0 {
			spent, limit, exceeded := budgetSvc.CheckBudget(
				c.Request.Context(), "key",
				fmt.Sprintf("%d", apiKey.ID),
				apiKey.BudgetPeriod, apiKey.MaxBudget,
			)
			if exceeded {
				c.Set("budget_exceeded", true)
				c.JSON(http.StatusTooManyRequests, gin.H{
					"type":  "error",
					"error": gin.H{"type": "budget_exceeded", "message": fmt.Sprintf("API key budget exceeded: %.4f/%.4f", spent, limit)},
				})
				c.Abort()
				return
			}
			c.Set("key_budget_period", apiKey.BudgetPeriod)
			scopes = append(scopes, service.BudgetScope{
				Scope: "key", ID: fmt.Sprintf("%d", apiKey.ID),
				Period: apiKey.BudgetPeriod, Limit: apiKey.MaxBudget,
			})
		}

		if apiKey.MaxCalls > 0 {
			// Atomic INCR-and-check at admission: the previous GET-based check
			// let N concurrent requests all pass before any of them completed
			// and incremented. Rejected requests are rolled back inside the
			// Lua script. The reservation is refunded post-completion for
			// paths that must not consume the call limit (skip_call_count).
			current, exceeded, pk := budgetSvc.ReserveCall(
				c.Request.Context(), fmt.Sprintf("%d", apiKey.ID),
				apiKey.CallPeriod, apiKey.MaxCalls,
			)
			if exceeded {
				c.Set("call_limit_exceeded", true)
				c.JSON(http.StatusTooManyRequests, gin.H{
					"type":  "error",
					"error": gin.H{"type": "call_limit_exceeded", "message": fmt.Sprintf("API key call limit exceeded: %d/%d", current, apiKey.MaxCalls)},
				})
				c.Abort()
				return
			}
			c.Set("call_reserved", true)
			c.Set("call_reserved_pk", pk)
		}

		if apiKey.TeamID != nil && *apiKey.TeamID > 0 {
			team, mode := teamCache.GetSafe(c.Request.Context(), *apiKey.TeamID)
			if mode == CacheModeUnavailable {
				// Review 2026-10-21: key-only admission on a metadata miss was an
				// ENFORCEMENT bypass (r61: 53 requests in a DB-outage window).
				// Cold cache + unreachable metadata = fail CLOSED.
				noteBudgetScopeDegrade("team", *apiKey.TeamID)
				c.Set("budget_metadata_unavailable", true)
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"type": "error",
					"error": gin.H{"type": "budget_metadata_unavailable", "message": "team budget metadata is temporarily unavailable (cold cache during a database outage); request rejected rather than admitted without the team limit"},
				})
				c.Abort()
				return
			}
			if mode == CacheModeStaleLKG {
				noteBudgetScopeDegradeStale("team", *apiKey.TeamID)
			}
			if team != nil && team.BudgetLimit > 0 {
				spent, limit, exceeded := budgetSvc.CheckBudget(
					c.Request.Context(), "team",
					fmt.Sprintf("%d", team.ID),
					team.BudgetPeriod, team.BudgetLimit,
				)
				if exceeded {
					c.Set("budget_exceeded", true)
					c.JSON(http.StatusTooManyRequests, gin.H{
						"type":  "error",
						"error": gin.H{"type": "budget_exceeded", "message": fmt.Sprintf("Team budget exceeded: %.4f/%.4f", spent, limit)},
					})
					c.Abort()
					return
				}
				c.Set("team_budget_period", team.BudgetPeriod)
				scopes = append(scopes, service.BudgetScope{
					Scope: "team", ID: fmt.Sprintf("%d", team.ID),
					Period: team.BudgetPeriod, Limit: team.BudgetLimit,
				})
			}
		}

		// 3. Org-level check
		if orgCache != nil {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				org, mode := orgCache.GetSafe(c.Request.Context(), orgID)
				if mode == CacheModeUnavailable {
					noteBudgetScopeDegrade("org", orgID)
					c.Set("budget_metadata_unavailable", true)
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"type": "error",
						"error": gin.H{"type": "budget_metadata_unavailable", "message": "organization budget metadata is temporarily unavailable (cold cache during a database outage); request rejected rather than admitted without the organization limit"},
					})
					c.Abort()
					return
				}
				if mode == CacheModeStaleLKG {
					noteBudgetScopeDegradeStale("org", orgID)
				}
				if org != nil && org.BudgetLimit > 0 {
					_, _, exceeded := budgetSvc.CheckBudget(c.Request.Context(), "org", fmt.Sprintf("%d", org.ID), org.BudgetPeriod, org.BudgetLimit)
					if exceeded {
						c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
							"type": "budget_exceeded", "message": "organization budget exceeded",
						}})
						c.Abort()
						return
					}
					c.Set("org_budget_period", org.BudgetPeriod)
					scopes = append(scopes, service.BudgetScope{
						Scope: "org", ID: fmt.Sprintf("%d", org.ID),
						Period: org.BudgetPeriod, Limit: org.BudgetLimit,
					})
				}
			}
		}

		c.Set("budget_scopes", scopes)
		c.Next()
	}
}

// AdminBudgetCheck checks team and org budget for admin/playground routes.
// It reads team_id/org_id from JWT context instead of api_key.
// Pass nil budgetSvc to disable (Community mode).
func AdminBudgetCheck(budgetSvc service.BudgetServiceInterface, teamCache *TeamCache, orgCache *OrgCache) gin.HandlerFunc {
	return func(c *gin.Context) {
		if budgetSvc == nil {
			c.Next()
			return
		}

		teamIDVal, _ := c.Get("team_id")
		tid, ok := teamIDVal.(int64)
		if ok && tid > 0 {
			team, mode := teamCache.GetSafe(c.Request.Context(), tid)
			if mode == CacheModeUnavailable {
				noteBudgetScopeDegrade("team", tid)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
					"type": "budget_metadata_unavailable", "message": "team budget metadata is temporarily unavailable; request rejected",
				}})
				c.Abort()
				return
			}
			if mode == CacheModeStaleLKG {
				noteBudgetScopeDegradeStale("team", tid)
			}
			if team != nil && team.BudgetLimit > 0 {
				spent, limit, exceeded := budgetSvc.CheckBudget(
					c.Request.Context(), "team",
					fmt.Sprintf("%d", team.ID),
					team.BudgetPeriod, team.BudgetLimit,
				)
				if exceeded {
					c.Set("budget_exceeded", true)
					c.JSON(http.StatusTooManyRequests, gin.H{
						"type":  "error",
						"error": gin.H{"type": "budget_exceeded", "message": fmt.Sprintf("Team budget exceeded: %.4f/%.4f", spent, limit)},
					})
					c.Abort()
					return
				}
				c.Set("team_budget_period", team.BudgetPeriod)
			}
		}

		// Org-level check
		if orgCache != nil {
			if orgID := c.GetInt64("org_id"); orgID != 0 {
				org, mode := orgCache.GetSafe(c.Request.Context(), orgID)
				if mode == CacheModeUnavailable {
					noteBudgetScopeDegrade("org", orgID)
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
						"type": "budget_metadata_unavailable", "message": "organization budget metadata is temporarily unavailable; request rejected",
					}})
					c.Abort()
					return
				}
				if mode == CacheModeStaleLKG {
					noteBudgetScopeDegradeStale("org", orgID)
				}
				if org != nil && org.BudgetLimit > 0 {
					_, _, exceeded := budgetSvc.CheckBudget(c.Request.Context(), "org", fmt.Sprintf("%d", org.ID), org.BudgetPeriod, org.BudgetLimit)
					if exceeded {
						c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
							"type": "budget_exceeded", "message": "organization budget exceeded",
						}})
						c.Abort()
						return
					}
					c.Set("org_budget_period", org.BudgetPeriod)
				}
			}
		}

		c.Next()
	}
}

// noteBudgetScopeDegradeStale counts enforcement on a last-known-good
// snapshot (DB unreachable, snapshot age <= ttl+staleWindow): the limit is
// STILL enforced and the scope IS reserved/settled — this is the
// availability-preserving branch of the fail-safe, distinct from the
// cold-cache rejection.
func noteBudgetScopeDegradeStale(scope string, id int64) {
	budgetScopeDegradeTotal.WithLabelValues(scope, "stale_lkg").Inc()
}
