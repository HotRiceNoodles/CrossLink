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
				}
			}
		}

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
