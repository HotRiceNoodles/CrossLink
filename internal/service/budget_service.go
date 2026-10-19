package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// F1′ request-idempotent reservation/settlement (2026-10-19 authorized).
// Key space (s = "<scope>:<targetID>", r = request_id, pk = period key):
//   budget:{s}:{pk}     enforcement counter (existing)
//   budgetset:{s}:{pk}  settled sum (audit/invariant)
//   budgetresv:{s}:{pk} outstanding reservation sum
//   resvmark:{s}:{r}    per-(scope,request) reservation marker, value = e, TTL
//   settledmark:{s}:{r} settle-once guard, TTL
//   resvexp             ZSET {expiry -> member} driving the orphan reaper
//
// Atomicity: reserve and settle are each ONE Lua script — the mark, the
// counters and the reaper zset move together or not at all. Idempotency:
// SET NX on the marks; a replayed reserve (response lost, any retry path)
// reports current state without re-incrementing, and a duplicate settle is
// a no-op. Exactness: settlement reads the per-request mark as ground truth
// — landed reservation settles (cost-e), un-landed settles full cost.
// Refund == settle with cost 0 (mark present → budget -= e).
const (
	budgetResvExpZSet = "resvexp"
	defaultMarkerTTL  = 24 * time.Hour
)

// budgetReserveMarkScript: KEYS = [resvmark, budget, budgetresv, resvexp];
// ARGV = [estimate, limit, budgetTTLsec, markTTLsec, expiryEpoch, member,
// estimateStr]. Returns {spent, limit, exceeded, reserved} where reserved=1
// means the reservation is held (created now, or already held on replay).
var budgetReserveMarkScript = redis.NewScript(`
local mark = KEYS[1]
local budget = KEYS[2]
local resv = KEYS[3]
local zset = KEYS[4]
local e = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttlBudget = tonumber(ARGV[3])
local ttlMark = tonumber(ARGV[4])
local expiry = tonumber(ARGV[5])
local member = ARGV[6]
if redis.call('SET', mark, ARGV[7], 'NX', 'EX', ttlMark) then
	redis.call('INCRBYFLOAT', budget, e)
	redis.call('INCRBYFLOAT', resv, e)
	redis.call('EXPIRE', resv, ttlBudget)
	redis.call('ZADD', zset, expiry, member)
	local spent = tonumber(redis.call('GET', budget)) or 0
	if spent > limit then
		redis.call('INCRBYFLOAT', budget, -e)
		redis.call('INCRBYFLOAT', resv, -e)
		redis.call('ZREM', zset, member)
		redis.call('DEL', mark)
		return {spent - e, limit, 1, 0}
	end
	redis.call('EXPIRE', budget, ttlBudget)
	return {spent, limit, 0, 1}
end
-- replay of an already-held reservation: idempotent, no re-increment
local cur = tonumber(redis.call('GET', budget)) or 0
return {cur, limit, 0, 1}
`)

// budgetSettleScript: KEYS = [settledmark, resvmark, budget, budgetresv,
// budgetset, resvexp]; ARGV = [cost, markTTLsec, scope, targetID, reqID,
// period, pk]. The reaper-zset member is rebuilt INSIDE the script from the
// mark value (no GET→script race). Returns 2 for a duplicate settle (no-op),
// 1 settled-against-landed-reservation, 0 reservation-never-landed (full
// cost).
var budgetSettleScript = redis.NewScript(`
local settledmark = KEYS[1]
local mark = KEYS[2]
local budget = KEYS[3]
local resv = KEYS[4]
local settled = KEYS[5]
local zset = KEYS[6]
local c = tonumber(ARGV[1])
local ttlMark = tonumber(ARGV[2])
local ttlBudget = tonumber(ARGV[8])
if not redis.call('SET', settledmark, '1', 'NX', 'EX', ttlMark) then
	return 2
end
redis.call('EXPIRE', settled, ttlBudget)
redis.call('EXPIRE', resv, ttlBudget)
local m = redis.call('GET', mark)
if m then
	local e = tonumber(m)
	redis.call('INCRBYFLOAT', budget, c - e)
	redis.call('INCRBYFLOAT', resv, -e)
	redis.call('DEL', mark)
	local member = ARGV[3] .. '\31' .. ARGV[4] .. '\31' .. ARGV[5] .. '\31' .. ARGV[6] .. '\31' .. ARGV[7] .. '\31' .. m
	redis.call('ZREM', zset, member)
	redis.call('INCRBYFLOAT', settled, c)
	return 1
end
redis.call('INCRBYFLOAT', budget, c)
redis.call('INCRBYFLOAT', settled, c)
return 0
`)

// budgetReapOrphanScript removes an EXPIRED reservation (crashed request,
// no settlement will ever arrive): budget/resv -= e, zset cleanup. A mark
// still present means the zset score ran ahead of the TTL — caller
// re-schedules it. KEYS = [resvmark, budget, budgetresv, resvexp];
// ARGV = [e, member]. Returns 1 reaped, 0 still-live.
var budgetReapOrphanScript = redis.NewScript(`
local mark = KEYS[1]
local budget = KEYS[2]
local resv = KEYS[3]
local zset = KEYS[4]
local e = tonumber(ARGV[1])
local member = ARGV[2]
if redis.call('EXISTS', mark) == 1 then
	return 0
end
redis.call('INCRBYFLOAT', budget, -e)
redis.call('INCRBYFLOAT', resv, -e)
redis.call('ZREM', zset, member)
return 1
`)

func resvMarkKey(scope, targetID, reqID string) string {
	return fmt.Sprintf("resvmark:%s:%s:%s", scope, targetID, reqID)
}

func settledMarkKey(scope, targetID, reqID string) string {
	return fmt.Sprintf("settledmark:%s:%s:%s", scope, targetID, reqID)
}

func budgetResvKey(scope, targetID, pk string) string {
	return fmt.Sprintf("budgetresv:%s:%s:%s", scope, targetID, pk)
}

func budgetSettledKey(scope, targetID, pk string) string {
	return fmt.Sprintf("budgetset:%s:%s:%s", scope, targetID, pk)
}

// resvExpMember encodes everything the reaper needs to undo a reservation.
func resvExpMember(scope, targetID, reqID, period, pk, estimate string) string {
	return scope + "\x1f" + targetID + "\x1f" + reqID + "\x1f" + period + "\x1f" + pk + "\x1f" + estimate
}

// redisWriteDefinitelyNotSent classifies a failed Redis command's side
// effects. Dial failures (connection never established) and pool-acquire
// timeouts mean the script was NEVER sent to the server. Everything else
// (i/o timeout, connection reset mid-flight, deadline after the write) is
// INDETERMINATE: the server may have applied the script and only the
// response was lost — a paused Redis, for example, executes queued commands
// the moment it unpauses. Reservation bookkeeping treats the two classes
// differently (see ReserveForRequest); it must never blind-refund the
// indeterminate class and never record the definitely-not-sent one.
// Var (not func) so tests can force the indeterminate classification.
var redisWriteDefinitelyNotSent = func(err error) bool {
	if errors.Is(err, redis.ErrPoolTimeout) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

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
	rdb       *redis.Client
	markerTTL time.Duration
}

func NewBudgetService(rdb *redis.Client) *BudgetService {
	return &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
}

// NewBudgetServiceWithMarkerTTL overrides the reservation/settlement marker
// TTL (config gateway.budget_marker_ttl; tests use short values to exercise
// expiry paths). Markers only outlive a request, so live-mark cardinality
// ≈ in-flight × scopes — the TTL bounds orphan cleanup latency.
func NewBudgetServiceWithMarkerTTL(rdb *redis.Client, ttl time.Duration) *BudgetService {
	if ttl <= 0 {
		ttl = defaultMarkerTTL
	}
	return &BudgetService{rdb: rdb, markerTTL: ttl}
}

// ReserveScopeMarked atomically reserves estimate for one scope under a
// request idempotency marker. written=true means the reservation is held
// (created, or already held by an earlier attempt whose response was lost —
// the replay branch reports state without re-incrementing). On Lua failure
// the same definitely-not-sent vs indeterminate classification as v2
// applies, but settlement no longer depends on that bookkeeping: it reads
// the mark as ground truth.
func (s *BudgetService) ReserveScopeMarked(ctx context.Context, scope, targetID, period, pk, reqID string, estimate, budgetLimit float64) (spent, limit float64, written, exceeded bool) {
	if budgetLimit <= 0 || estimate <= 0 {
		return 0, budgetLimit, false, false
	}
	if reqID == "" {
		// No request identity (legacy callers): fall back to the v2 path.
		return s.reserveBudgetKeyed(ctx, scope, targetID, period, pk, estimate, budgetLimit)
	}
	bk := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	rk := budgetResvKey(scope, targetID, pk)
	mk := resvMarkKey(scope, targetID, reqID)
	now := time.Now()
	member := resvExpMember(scope, targetID, reqID, period, pk, fmt.Sprintf("%.9f", estimate))
	res, err := budgetReserveMarkScript.Run(ctx, s.rdb,
		[]string{mk, bk, rk, budgetResvExpZSet},
		estimate, budgetLimit,
		int(PeriodTTL(period).Seconds()), int(s.markerTTL.Seconds()),
		now.Add(s.markerTTL).Unix(), member,
		fmt.Sprintf("%.9f", estimate),
	).Float64Slice()
	if err != nil {
		if redisWriteDefinitelyNotSent(err) {
			slog.Warn("budget reserve(idempotent) definitely NOT sent; admission falls back to non-atomic check",
				"key", bk, "request_id", reqID, "error", err)
		} else {
			slog.Warn("budget reserve(idempotent) result INDETERMINATE; settlement will reconcile via the request mark",
				"key", bk, "request_id", reqID, "error", err)
		}
		sp, lim, exc := s.CheckBudget(ctx, scope, targetID, period, budgetLimit)
		return sp, lim, false, exc
	}
	return res[0], res[1], res[3] == 1, res[2] == 1
}

// Bounded in-process audit-replay queue for settles that exhausted their
// inline retries (r33/r33b finding: a settlement whose whole 30s caller
// window falls inside a longer Redis outage is otherwise lost until the
// hourly DB raise). The 24h mark TTL makes a LATE settle exact and safe,
// so replaying is pure recovery. Overflow beyond the queue cap gives up
// (hourly-healable under-count); a process crash loses the queue — the
// mark+reaper+DB-raise chain covers that path (documented residual).
type settleRetryTask struct {
	scope, targetID, period, pk, reqID string
	cost                              float64
	deadline                          time.Time
}

const (
	settleRetryQueueCap   = 65536
	settleRetryHorizon    = 10 * time.Minute
	settleRetryMaxBackoff = 15 * time.Second
)

var (
	settleRetryQ    = make(chan settleRetryTask, settleRetryQueueCap)
	settleRetryOnce sync.Once
)

func (s *BudgetService) enqueueSettleRetry(t settleRetryTask) bool {
	select {
	case settleRetryQ <- t:
		settleRetryOnce.Do(func() { go s.drainSettleRetries() })
		return true
	default:
		return false
	}
}

func (s *BudgetService) drainSettleRetries() {
	for t := range settleRetryQ {
		backoff := 2 * time.Second
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := budgetSettleScript.Run(ctx, s.rdb,
				[]string{settledMarkKey(t.scope, t.targetID, t.reqID),
					resvMarkKey(t.scope, t.targetID, t.reqID),
					fmt.Sprintf("budget:%s:%s:%s", t.scope, t.targetID, t.pk),
					budgetResvKey(t.scope, t.targetID, t.pk),
					budgetSettledKey(t.scope, t.targetID, t.pk),
					budgetResvExpZSet},
				t.cost, int(s.markerTTL.Seconds()), t.scope, t.targetID, t.reqID, t.period, t.pk,
				int(PeriodTTL(t.period).Seconds()),
			).Int()
			cancel()
			if err == nil {
				slog.Info("budget settle audit-replay RECOVERED",
					"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost)
				break
			}
			if time.Now().After(t.deadline) {
				slog.Warn("budget settle audit-replay EXPIRED (under-count direction; hourly DB-based calibration heals)",
					"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost, "error", err)
				break
			}
			time.Sleep(backoff)
			if backoff < settleRetryMaxBackoff {
				backoff *= 2
			}
		}
	}
}

// SettleScope settles one scope for a request EXACTLY ONCE, reconciling
// against the reservation mark: mark present → the reserve demonstrably
// landed, apply (cost-e) and release it; mark absent → the reserve never
// landed (definitely-not-sent, or its TTL expired and the reaper already
// removed it) → apply full cost. cost==0 with a mark present is a REFUND.
//
// Retry ladder (r33/r33b findings): the settledmark NX makes every retry
// idempotent, so (1) inline bounded retries with backoff inside the caller
// context, then (2) a bounded in-process audit-replay queue with a 10-minute
// horizon — a settlement whose caller window falls entirely inside a longer
// Redis outage is still recovered once Redis returns. Beyond the horizon
// (or on process crash) the loss stays in the under-count direction the
// hourly DB-based calibration raise heals.
func (s *BudgetService) SettleScope(ctx context.Context, scope, targetID, period, pk, reqID string, cost float64) {
	if reqID == "" {
		// Legacy path (no request identity): full-cost report.
		s.ReportUsage(ctx, scope, targetID, period, cost)
		return
	}
	mk := resvMarkKey(scope, targetID, reqID)
	run := func() (int, error) {
		return budgetSettleScript.Run(ctx, s.rdb,
			[]string{settledMarkKey(scope, targetID, reqID), mk,
				fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk),
				budgetResvKey(scope, targetID, pk),
				budgetSettledKey(scope, targetID, pk),
				budgetResvExpZSet},
			cost, int(s.markerTTL.Seconds()), scope, targetID, reqID, period, pk,
			int(PeriodTTL(period).Seconds()),
		).Int()
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		res, err := run()
		if err == nil {
			slog.Debug("budget settle applied", "scope", scope, "target", targetID,
				"request_id", reqID, "cost", cost, "mode", res, "attempt", attempt)
			return
		}
		if attempt >= 4 || ctx.Err() != nil {
			if ok := s.enqueueSettleRetry(settleRetryTask{
				scope: scope, targetID: targetID, period: period, pk: pk,
				reqID: reqID, cost: cost, deadline: time.Now().Add(settleRetryHorizon),
			}); ok {
				slog.Warn("budget settle deferred to audit-replay queue (inline retries exhausted)",
					"scope", scope, "target", targetID, "request_id", reqID, "cost", cost, "error", err)
			} else {
				slog.Warn("budget settle LOST (replay queue full; under-count direction; hourly DB-based calibration heals)",
					"scope", scope, "target", targetID, "request_id", reqID, "cost", cost, "error", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			if ok := s.enqueueSettleRetry(settleRetryTask{
				scope: scope, targetID: targetID, period: period, pk: pk,
				reqID: reqID, cost: cost, deadline: time.Now().Add(settleRetryHorizon),
			}); ok {
				slog.Warn("budget settle deferred to audit-replay queue (caller context done)",
					"scope", scope, "request_id", reqID, "cost", cost)
			}
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// ReapExpiredReservations removes reservations whose marks expired without a
// settlement (gateway crash mid-request): budget/resv -= e. The crashed
// request's true cost is restored by the next DB-based raise from its
// WAL→usage_logs row. Live marks found by score are re-scheduled. Returns
// the number of orphans reaped.
func (s *BudgetService) ReapExpiredReservations(ctx context.Context, now time.Time) int {
	reaped := 0
	for {
		members, err := s.rdb.ZRangeByScore(ctx, budgetResvExpZSet, &redis.ZRangeBy{
			Min: "-inf", Max: fmt.Sprintf("%d", now.Unix()), Count: 500,
		}).Result()
		if err != nil {
			slog.Warn("budget reaper: zrange failed", "error", err)
			return reaped
		}
		if len(members) == 0 {
			return reaped
		}
		for _, m := range members {
			parts := strings.SplitN(m, "\x1f", 6)
			if len(parts) != 6 {
				// Malformed member: drop it so it cannot wedge the zset.
				s.rdb.ZRem(ctx, budgetResvExpZSet, m)
				continue
			}
			scope, targetID, reqID, _, pk, eStr := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
			e, _ := strconv.ParseFloat(eStr, 64)
			res, err := budgetReapOrphanScript.Run(ctx, s.rdb,
				[]string{resvMarkKey(scope, targetID, reqID),
					fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk),
					budgetResvKey(scope, targetID, pk),
					budgetResvExpZSet},
				e, m,
			).Int()
			if err != nil {
				slog.Warn("budget reaper: reap failed", "member", m, "error", err)
				continue
			}
			if res == 1 {
				reaped++
				slog.Warn("budget reaper: removed EXPIRED reservation (crashed/unsettled request); its ledger row, if any, is restored by the next DB-based calibration raise",
					"scope", scope, "target", targetID, "request_id", reqID, "estimate", e)
			} else {
				// Mark still live — score ran ahead of the TTL; re-schedule.
				s.rdb.ZAdd(ctx, budgetResvExpZSet, redis.Z{
					Score:  float64(time.Now().Add(s.markerTTL).Unix()),
					Member: m,
				})
			}
		}
		if len(members) < 500 {
			return reaped
		}
	}
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
	spent, limit, _, exceeded := s.reserveBudgetKeyed(ctx, scope, targetID, period, PeriodKey(period), cost, budgetLimit)
	return spent, limit, exceeded
}

// reserveBudgetKeyed is ReserveBudget with the period key supplied by the
// caller (resolved once at reserve time), so settlement/refund can land in
// the same period even when the request crosses a UTC period boundary.
// Returns (spent, limit, written, exceeded): `written` is true when the
// atomic increment should be treated as APPLIED for settlement purposes —
// either it demonstrably applied, or the failure is INDETERMINATE (applied
// but response lost), where treating it as applied is the only choice whose
// divergence lands in the under-count direction the hourly raise-only
// calibration can heal (see ReserveForRequest). DEFINITELY-NOT-SENT
// failures (dial/pool-acquire) return written=false: the increment never
// reached the server, so no reservation may be recorded.
func (s *BudgetService) reserveBudgetKeyed(ctx context.Context, scope, targetID, period, pk string, cost, budgetLimit float64) (float64, float64, bool, bool) {
	if budgetLimit <= 0 || cost <= 0 {
		return 0, budgetLimit, false, false
	}
	key := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	ttl := int(PeriodTTL(period).Seconds())

	result, err := budgetReserveScript.Run(ctx, s.rdb, []string{key},
		cost, budgetLimit, ttl,
	).Float64Slice()
	if err != nil {
		if redisWriteDefinitelyNotSent(err) {
			slog.Warn("budget reserve lua definitely NOT sent (dial/pool-acquire failed); no reservation recorded, settlement reports full cost (exact)",
				"key", key, "error", err)
			spent, limit, exceeded := s.CheckBudget(ctx, scope, targetID, period, budgetLimit)
			return spent, limit, false, exceeded
		}
		// Indeterminate: the script MAY have been applied with only the
		// response lost (observed live: a paused Redis executes queued
		// commands on unpause). Recording the reservation keeps settlement
		// at (cost-reserved): exact if it landed; if it never landed the
		// counter ends short by the estimate — the UNDER-count direction,
		// which the hourly raise-only calibration heals. NOT recording
		// would double-count the landed case (reserve + full cost) and
		// produce an OVER-count that raise-only calibration can never
		// remove (persists until period roll).
		slog.Warn("budget reserve lua result INDETERMINATE (may have applied; response lost); reservation RECORDED so settlement lands (cost-reserved) — divergence stays in the calibration-healable direction",
			"key", key, "error", err)
		spent, limit, exceeded := s.CheckBudget(ctx, scope, targetID, period, budgetLimit)
		return spent, limit, true, exceeded
	}
	spent := result[0]
	limit := result[1]
	exceeded := result[2] == 1
	return spent, limit, true, exceeded
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
		return
	}
	// Debug-only settlement trace (review r18s forensics: a whole exit-wave
	// cohort's adjustments once vanished with no error logged; this line makes
	// such gaps attributable when CL_LOG_LEVEL=debug is enabled on a rig).
	slog.Debug("budget delta applied", "key", key, "delta", delta)
}

// ReserveForRequest reserves an estimated cost against each budget scope in
// order (key → team → org). With a non-empty reqID it uses the F1′
// REQUEST-IDEMPOTENT path: every scope's reservation is an atomic
// mark+counters Lua keyed by (scope, request_id) — replays are no-ops, and
// settlement later reads the mark as ground truth, so the v2
// indeterminate/definitely-not-sent bookkeeping below no longer gates
// correctness (kept for the legacy no-reqID path and for observability).
// On exceed, lower levels are refunded via SettleScope(cost=0) — the
// refund-as-settle-zero form of the same idempotent script.
func (s *BudgetService) ReserveForRequest(ctx context.Context, scopes []BudgetScope, estimate float64, reqID string) (*BudgetReservations, string) {
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
		var written, exceeded bool
		if reqID != "" {
			_, _, written, exceeded = s.ReserveScopeMarked(ctx, sc.Scope, sc.ID, sc.Period, pk, reqID, estimate, sc.Limit)
		} else {
			_, _, written, exceeded = s.reserveBudgetKeyed(ctx, sc.Scope, sc.ID, sc.Period, pk, estimate, sc.Limit)
		}
		if exceeded {
			// Refund the levels reserved before this one; the script already
			// rolled back this level's own increment.
			for j := 0; j < i; j++ {
				if prev := res.Reserved[scopes[j].Scope]; prev > 0 {
					if reqID != "" {
						s.SettleScope(ctx, scopes[j].Scope, scopes[j].ID, scopes[j].Period, res.PeriodKeys[scopes[j].Scope], reqID, 0)
					} else {
						s.applyBudgetDeltaKeyed(ctx, scopes[j].Scope, scopes[j].ID, scopes[j].Period, res.PeriodKeys[scopes[j].Scope], -prev)
					}
					delete(res.Reserved, scopes[j].Scope)
				}
			}
			return res, sc.Scope
		}
		if written {
			res.Reserved[sc.Scope] = estimate
			res.PeriodKeys[sc.Scope] = pk
		}
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
