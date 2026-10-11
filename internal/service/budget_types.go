package service

import "context"

// BudgetServiceInterface decouples middleware from BudgetService implementation.
// Community passes nil (middleware short-circuits with c.Next()).
// Commercial injects real BudgetService.
type BudgetServiceInterface interface {
	CheckBudget(ctx context.Context, scope, targetID, period string, budgetLimit float64) (spent, limit float64, exceeded bool)
	ReportUsage(ctx context.Context, scope, targetID, period string, cost float64)
	AdjustBudget(ctx context.Context, scope, targetID, period string, delta float64)
	// AdjustBudgetKeyed adjusts with an explicit period key (resolved at
	// reservation time) so refunds land in the period they were reserved
	// against; falls back to deriving the key when empty.
	AdjustBudgetKeyed(ctx context.Context, scope, targetID, period, periodKey string, delta float64)
	CheckCallLimit(ctx context.Context, keyID, period string, maxCalls int) (current int, exceeded bool)
	// ReserveCall atomically counts one call at admission and reports the
	// period key used, so a later RefundCall targets the same counter.
	ReserveCall(ctx context.Context, keyID, period string, maxCalls int) (current int, exceeded bool, periodKey string)
	RefundCall(ctx context.Context, keyID, periodKey string)
	ReportCallUsage(ctx context.Context, keyID, period string)
}

// BudgetScope identifies one budget counter to reserve against during
// pre-request reservation. Ordered key → team → org.
type BudgetScope struct {
	Scope  string // "key" | "team" | "org"
	ID     string
	Period string
	Limit  float64 // 0 = no limit at this level (skip)
}

// BudgetReservations records the amount reserved per scope during pre-request
// reservation, so ReportBudgetUsage can reconcile each counter by (actual - reserved)
// instead of adding the full actual cost on top of the reservation.
type BudgetReservations struct {
	Reserved map[string]float64 // scope -> reserved amount
	// PeriodKeys records the period key each counter was reserved against,
	// resolved once at reserve time. Settlement and refunds use it so a
	// request that crosses a UTC period boundary reconciles the counter it
	// actually reserved, not the (new) one computed at settlement time.
	PeriodKeys map[string]string // scope -> period key
}

// BudgetAlertServiceInterface decouples middleware from BudgetAlertService implementation.
// Community passes nil (middleware skips alert checks).
// Commercial injects real BudgetAlertService.
type BudgetAlertServiceInterface interface {
	CheckAndAlert(ctx context.Context, scope, targetID, periodType string, spent, budget float64)
}
