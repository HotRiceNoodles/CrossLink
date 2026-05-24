package service

import "context"

// BudgetServiceInterface decouples middleware from BudgetService implementation.
// Community passes nil (middleware short-circuits with c.Next()).
// Commercial injects real BudgetService.
type BudgetServiceInterface interface {
	CheckBudget(ctx context.Context, scope, targetID, period string, budgetLimit float64) (spent, limit float64, exceeded bool)
	ReportUsage(ctx context.Context, scope, targetID, period string, cost float64)
}

// BudgetAlertServiceInterface decouples middleware from BudgetAlertService implementation.
// Community passes nil (middleware skips alert checks).
// Commercial injects real BudgetAlertService.
type BudgetAlertServiceInterface interface {
	CheckAndAlert(ctx context.Context, scope, targetID, periodType string, spent, budget float64)
}
