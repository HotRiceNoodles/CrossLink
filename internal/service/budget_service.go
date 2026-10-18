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
// Boundary semantics: spent == limit is ALLOWED (a request may spend its exact
// remaining budget) — consistent with the TPM and call-limit scripts.
var budgetReserveScript = redis.NewScript(`
	local key = KEYS[1]
	local cost = tonumber(ARGV[1])
	local limit = tonumber(ARGV[2])
	local ttl = tonumber(ARGV[3])
	local spent = tonumber(redis.call("INCRBYFLOAT", key, cost))
	if spent == cost then
		redis.call("EXPIRE", key, ttl)
	end
	if spent > limit then
		redis.call("INCRBYFLOAT", key, -cost)
		return { spent - cost, limit, 1 }
	end
	return { spent, limit, 0 }
`)

// Lua script: atomically increment the call counter and check against the
// limit. On exceed it rolls the increment back so rejected requests don't
// consume the call limit. Mirrors budgetReserveScript for the calls dimension,
// closing the check-then-act race between GET-based CheckCallLimit and the
// post-completion INCR.
var callReserveScript = redis.NewScript(`
	local count = redis.call("INCR", KEYS[1])
	if count == 1 then
		redis.call("EXPIRE", KEYS[1], ARGV[2])
	end
	if count > tonumber(ARGV[1]) then
		redis.call("DECR", KEYS[1])
		return { count - 1, 1 }
	end
	return { count, 0 }
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

// PeriodStart returns the UTC start of the CURRENT period — the matching
// lower bound for queries that must land in the same bucket as PeriodKey.
// Calibration SUMs use this so a recalibrate never charges an org for its
// entire history (U-28).
func PeriodStart(period string) time.Time {
	return periodStartAt(period, time.Now().UTC())
}

func periodStartAt(period string, now time.Time) time.Time {
	now = now.UTC()
	switch period {
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "weekly":
		// Jan 4 is always in ISO week 1; walk to that week's Monday, then
		// forward (w-1) weeks.
		jan4 := time.Date(now.Year(), 1, 4, 0, 0, 0, 0, time.UTC)
		_, wk1 := jan4.ISOWeek()
		_, w := now.ISOWeek()
		monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
		return monday.AddDate(0, 0, (w-wk1)*7)
	default: // monthly
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
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
	// Boundary: spent == limit is still allowed (matches budgetReserveScript,
	// the TPM and the call-limit scripts).
	return val, budgetLimit, val > budgetLimit
}

// ReserveBudget atomically increments the budget counter and checks against the limit.
// Returns (new_spent, limit, exceeded). Use this when cost is known at check time.
func (s *BudgetService) ReserveBudget(ctx context.Context, scope, targetID, period string, cost, budgetLimit float64) (float64, float64, bool) {
	return s.reserveBudgetKeyed(ctx, scope, targetID, period, PeriodKey(period), cost, budgetLimit)
}

// reserveBudgetKeyed is ReserveBudget with the period key supplied by the
// caller (resolved once at reserve time), so settlement/refund can land in the
// same period even when the request crosses a UTC period boundary.
func (s *BudgetService) reserveBudgetKeyed(ctx context.Context, scope, targetID, period, pk string, cost, budgetLimit float64) (float64, float64, bool) {
	if budgetLimit <= 0 || cost <= 0 {
		return 0, budgetLimit, false
	}
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
	s.applyBudgetDeltaKeyed(ctx, scope, targetID, period, PeriodKey(period), cost)
}

// AdjustBudget applies a signed delta to a budget counter, refreshing the TTL.
// Used by ReportBudgetUsage to reconcile a pre-request reservation against the
// actual cost: delta = actual - reserved. A negative delta refunds an
// over-reservation (or the full reservation on a zero-cost / failed request).
func (s *BudgetService) AdjustBudget(ctx context.Context, scope, targetID, period string, delta float64) {
	if delta == 0 {
		return
	}
	s.applyBudgetDeltaKeyed(ctx, scope, targetID, period, PeriodKey(period), delta)
}

// AdjustBudgetKeyed is AdjustBudget with an explicit period key — the one
// resolved when the reservation was made, so refunds land in the period they
// were reserved against even when the request crosses a period boundary.
func (s *BudgetService) AdjustBudgetKeyed(ctx context.Context, scope, targetID, period, periodKey string, delta float64) {
	if delta == 0 || periodKey == "" {
		s.AdjustBudget(ctx, scope, targetID, period, delta)
		return
	}
	s.applyBudgetDeltaKeyed(ctx, scope, targetID, period, periodKey, delta)
}

// applyBudgetDeltaKeyed applies a non-zero delta to a budget counter and
// refreshes its TTL. Shared body of ReportUsage (positive cost) and
// AdjustBudget (signed delta, including negative refunds).
func (s *BudgetService) applyBudgetDeltaKeyed(ctx context.Context, scope, targetID, period, pk string, delta float64) {
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	ttl := PeriodTTL(period)
	pipe := s.rdb.Pipeline()
	pipe.IncrByFloat(ctx, key, delta)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("budget delta redis pipeline failed", "key", key, "delta", delta, "error", err)
	}
}

// ReserveForRequest atomically reserves an estimated cost against each budget
// scope in order (key → team → org). If a level would exceed its limit, that
// level's reservation is rolled back by the Lua script and all lower levels are
// refunded here; the returned exceededScope is non-empty. On success, every
// reserved amount is recorded in the returned BudgetReservations for later
// reconciliation. A non-positive estimate yields a no-op reservation.
func (s *BudgetService) ReserveForRequest(ctx context.Context, scopes []BudgetScope, estimate float64) (*BudgetReservations, string) {
	res := &BudgetReservations{
		Reserved:   map[string]float64{},
		PeriodKeys: map[string]string{},
	}
	if estimate <= 0 || len(scopes) == 0 {
		return res, ""
	}
	for i, sc := range scopes {
		if sc.Limit <= 0 {
			continue
		}
		// Resolve the period key once, here: reconciliation must adjust the
		// same counter even if the request crosses a UTC period boundary.
		pk := PeriodKey(sc.Period)
		_, _, exceeded := s.reserveBudgetKeyed(ctx, sc.Scope, sc.ID, sc.Period, pk, estimate, sc.Limit)
		if exceeded {
			// Refund the levels reserved before this one; the script already
			// rolled back this level's own increment. Clear their entries so the
			// returned reservations reflect reality (nothing is held).
			for j := 0; j < i; j++ {
				if prev := res.Reserved[scopes[j].Scope]; prev > 0 {
					s.applyBudgetDeltaKeyed(ctx, scopes[j].Scope, scopes[j].ID, scopes[j].Period, res.PeriodKeys[scopes[j].Scope], -prev)
					delete(res.Reserved, scopes[j].Scope)
				}
			}
			return res, sc.Scope
		}
		res.Reserved[sc.Scope] = estimate
		res.PeriodKeys[sc.Scope] = pk
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

// ReserveCall atomically increments the call counter and checks against the
// limit at admission time, closing the check-then-act race that the GET-based
// CheckCallLimit plus post-completion ReportCallUsage had (N concurrent
// requests all saw "under limit" and were all admitted). Returns the reserved
// period key so a later refund lands on the same counter.
func (s *BudgetService) ReserveCall(ctx context.Context, keyID, period string, maxCalls int) (int, bool, string) {
	if maxCalls <= 0 {
		return 0, false, ""
	}
	pk := PeriodKey(period)
	key := fmt.Sprintf("calls:key:%s:%s", keyID, pk)
	ttl := int(PeriodTTL(period).Seconds())

	result, err := callReserveScript.Run(ctx, s.rdb, []string{key}, maxCalls, ttl).Int64Slice()
	if err != nil {
		slog.Warn("call reserve lua failed, falling back to non-atomic check", "key", key, "error", err)
		current, exceeded := s.CheckCallLimit(ctx, keyID, period, maxCalls)
		return current, exceeded, pk
	}
	return int(result[0]), result[1] == 1, pk
}

// RefundCall rolls back one admission-time call reservation (paths that must
// not consume the call limit: video status polling, idempotent replays,
// pre-upstream rejections). periodKey must be the one returned by ReserveCall.
func (s *BudgetService) RefundCall(ctx context.Context, keyID, periodKey string) {
	if periodKey == "" {
		return
	}
	key := fmt.Sprintf("calls:key:%s:%s", keyID, periodKey)
	if err := s.rdb.Decr(ctx, key).Err(); err != nil {
		slog.Warn("call refund failed", "key", key, "error", err)
	}
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
