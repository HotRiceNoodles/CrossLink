package middleware

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/service"
)

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
			current, exceeded := budgetSvc.CheckCallLimit(
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
		}

		if apiKey.TeamID != nil && *apiKey.TeamID > 0 {
			team := teamCache.Get(c.Request.Context(), *apiKey.TeamID)
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
				org := orgCache.Get(c.Request.Context(), orgID)
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
			team := teamCache.Get(c.Request.Context(), tid)
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
				org := orgCache.Get(c.Request.Context(), orgID)
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
