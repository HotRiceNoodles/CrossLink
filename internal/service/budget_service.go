package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script: atomically increment budget counter and return (new_spent, limit, exceeded).
// Returns: new spent amount, whether it exceeds the limit.
// The key gets a TTL only if newly created.
var budgetReserveScript = redis.NewScript(`
	local key = KEYS[1]
	local cost = tonumber(ARGV[1])
	local limit = tonumber(ARGV[2])
	local ttl = tonumber(ARGV[3])
	local spent = redis.call("INCRBYFLOAT", key, cost)
	if spent == cost then
		redis.call("EXPIRE", key, ttl)
	end
	return { spent, limit, spent >= limit and 1 or 0 }
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

func (s *BudgetService) GetCurrentSpent(ctx context.Context, scope, targetID, period string) float64 {
	pk := PeriodKey(period)
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	val, err := s.rdb.Get(ctx, key).Float64()
	if err != nil {
		return 0
	}
	return val
}
