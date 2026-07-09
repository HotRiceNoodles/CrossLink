package service

import "context"

// BudgetServiceInterface decouples middleware from BudgetService implementation.
// Community passes nil (middleware short-circuits with c.Next()).
// Commercial injects real BudgetService.
type BudgetServiceInterface interface {
	CheckBudget(ctx context.Context, scope, targetID, period string, budgetLimit float64) (spent, limit float64, exceeded bool)
	ReportUsage(ctx context.Context, scope, targetID, period string, cost float64)
	AdjustBudget(ctx context.Context, scope, targetID, period string, delta float64)
	CheckCallLimit(ctx context.Context, keyID, period string, maxCalls int) (current int, exceeded bool)
	ReportCallUsage(ctx context.Context, keyID, period string)
}

// BudgetScope identifies one budget counter to reserve against during
// pre-request reservation. Ordered key → team → org.
type BudgetScope struct {
	Scope  string  // "key" | "team" | "org"
	ID     string
	Period string
	Limit  float64 // 0 = no limit at this level (skip)
}

// BudgetReservations records the amount reserved per scope during pre-request
// reservation, so ReportBudgetUsage can reconcile each counter by (actual - reserved)
// instead of adding the full actual cost on top of the reservation.
type BudgetReservations struct {
	Reserved map[string]float64 // scope -> reserved amount
}

// BudgetAlertServiceInterface decouples middleware from BudgetAlertService implementation.
// Community passes nil (middleware skips alert checks).
// Commercial injects real BudgetAlertService.
type BudgetAlertServiceInterface interface {
	CheckAndAlert(ctx context.Context, scope, targetID, periodType string, spent, budget float64)
}
