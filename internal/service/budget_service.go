package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script: atomically increment budget counter and check against the limit.
// On exceed it ROLLS BACK the increment so rejected requests don't consume
// budget (otherwise a burst of over-limit requests would permanently inflate the
// counter). Returns (current_spent_after_decision, limit, exceeded).
var budgetReserveScript = redis.NewScript(`
	local key = KEYS[1]
	local cost = tonumber(ARGV[1])
	local limit = tonumber(ARGV[2])
	local ttl = tonumber(ARGV[3])
	local spent = tonumber(redis.call("INCRBYFLOAT", key, cost))
	if spent == cost then
		redis.call("EXPIRE", key, ttl)
	end
	if spent >= limit then
		redis.call("INCRBYFLOAT", key, -cost)
		return { spent - cost, limit, 1 }
	end
	return { spent, limit, 0 }
`)

type BudgetService struct {
	rdb *redis.Client
}

func NewBudgetService(rdb *redis.Client) *BudgetService {
	return &BudgetService{rdb: rdb}
}

func PeriodKey(period string) string {
	now := time.Now().UTC()
	switch period {
	case "daily":
		return now.Format("2006-01-02")
	case "weekly":
		y, w := now.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case "monthly":
		return now.Format("2006-01")
	default:
		return now.Format("2006-01")
	}
}

func PeriodTTL(period string) time.Duration {
	switch period {
	case "daily":
		return 48 * time.Hour
	case "weekly":
		return 14 * 24 * time.Hour
	case "monthly":
		return 62 * 24 * time.Hour
	default:
		return 62 * 24 * time.Hour
	}
}

func (s *BudgetService) CheckBudget(ctx context.Context, scope, targetID, period string, budgetLimit float64) (float64, float64, bool) {
	if budgetLimit <= 0 {
		return 0, 0, false
	}
	pk := PeriodKey(period)
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	val, err := s.rdb.Get(ctx, key).Float64()
	if err != nil {
		return 0, budgetLimit, false
	}
	return val, budgetLimit, val >= budgetLimit
}

// ReserveBudget atomically increments the budget counter and checks against the limit.
// Returns (new_spent, limit, exceeded). Use this when cost is known at check time.
func (s *BudgetService) ReserveBudget(ctx context.Context, scope, targetID, period string, cost, budgetLimit float64) (float64, float64, bool) {
	if budgetLimit <= 0 || cost <= 0 {
		return 0, budgetLimit, false
	}
	pk := PeriodKey(period)
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	ttl := int(PeriodTTL(period).Seconds())

	result, err := budgetReserveScript.Run(ctx, s.rdb, []string{key},
		cost, budgetLimit, ttl,
	).Float64Slice()
	if err != nil {
		slog.Warn("budget reserve lua failed, falling back to non-atomic check", "key", key, "error", err)
		return s.CheckBudget(ctx, scope, targetID, period, budgetLimit)
	}
	spent := result[0]
	limit := result[1]
	exceeded := result[2] == 1
	return spent, limit, exceeded
}

func (s *BudgetService) ReportUsage(ctx context.Context, scope, targetID, period string, cost float64) {
	if cost <= 0 {
		return
	}
	pk := PeriodKey(period)
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	ttl := PeriodTTL(period)
	pipe := s.rdb.Pipeline()
	pipe.IncrByFloat(ctx, key, cost)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("budget report redis pipeline failed", "key", key, "error", err)
	}
}

// AdjustBudget applies a delta (positive or negative) to a budget counter,
// refreshing the TTL. Used by ReportBudgetUsage to reconcile a pre-request
// reservation against the actual cost: delta = actual - reserved. A negative
// delta refunds an over-reservation (or the full reservation on a zero-cost /
// failed request).
func (s *BudgetService) AdjustBudget(ctx context.Context, scope, targetID, period string, delta float64) {
	if delta == 0 {
		return
	}
	pk := PeriodKey(period)
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	ttl := PeriodTTL(period)
	pipe := s.rdb.Pipeline()
	pipe.IncrByFloat(ctx, key, delta)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("budget adjust redis pipeline failed", "key", key, "delta", delta, "error", err)
	}
}

// ReserveForRequest atomically reserves an estimated cost against each budget
// scope in order (key → team → org). If a level would exceed its limit, that
// level's reservation is rolled back by the Lua script and all lower levels are
// refunded here; the returned exceededScope is non-empty. On success, every
// reserved amount is recorded in the returned BudgetReservations for later
// reconciliation. A non-positive estimate yields a no-op reservation.
func (s *BudgetService) ReserveForRequest(ctx context.Context, scopes []BudgetScope, estimate float64) (*BudgetReservations, string) {
	res := &BudgetReservations{Reserved: map[string]float64{}}
	if estimate <= 0 || len(scopes) == 0 {
		return res, ""
	}
	for i, sc := range scopes {
		if sc.Limit <= 0 {
			continue
		}
		_, _, exceeded := s.ReserveBudget(ctx, sc.Scope, sc.ID, sc.Period, estimate, sc.Limit)
		if exceeded {
			// Refund the levels reserved before this one; the script already
			// rolled back this level's own increment. Clear their entries so the
			// returned reservations reflect reality (nothing is held).
			for j := 0; j < i; j++ {
				if prev := res.Reserved[scopes[j].Scope]; prev > 0 {
					s.AdjustBudget(ctx, scopes[j].Scope, scopes[j].ID, scopes[j].Period, -prev)
					delete(res.Reserved, scopes[j].Scope)
				}
			}
			return res, sc.Scope
		}
		res.Reserved[sc.Scope] = estimate
	}
	return res, ""
}

func (s *BudgetService) CheckCallLimit(ctx context.Context, keyID, period string, maxCalls int) (int, bool) {
	if maxCalls <= 0 {
		return 0, false
	}
	pk := PeriodKey(period)
	key := fmt.Sprintf("calls:key:%s:%s", keyID, pk)
	val, err := s.rdb.Get(ctx, key).Int()
	if err != nil {
		return 0, false
	}
	return val, val >= maxCalls
}

func (s *BudgetService) ReportCallUsage(ctx context.Context, keyID, period string) {
	pk := PeriodKey(period)
	key := fmt.Sprintf("calls:key:%s:%s", keyID, pk)
	ttl := PeriodTTL(period)
	pipe := s.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("call count report redis pipeline failed", "key", key, "error", err)
	}
}

func (s *BudgetService) GetCurrentSpent(ctx context.Context, scope, targetID, period string) float64 {
	pk := PeriodKey(period)
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	val, err := s.rdb.Get(ctx, key).Float64()
	if err != nil {
		return 0
	}
	return val
}
