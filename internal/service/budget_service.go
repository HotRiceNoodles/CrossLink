package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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
	// settleLedgerZSet is the REDIS-PERSISTENT audit-replay ledger for settles
	// that exhausted inline retries (r34: the in-process queue alone lost
	// tasks on crash/full). Members are re-driven idempotently by the ledger
	// worker; the settledmark guard makes every re-drive a no-op or an exact
	// settle, never a double apply.
	settleLedgerZSet = "settleretry"
)

// budgetReserveMarkScript: KEYS = [resvmark, budget, budgetresv, resvexp,
// settledmark]; ARGV = [estimate, limit, budgetTTLsec, markTTLsec,
// expiryEpoch, member, estimateStr]. Returns {spent, limit, exceeded,
// reserved} where reserved=1 means the reservation is held (created now, or
// already held on replay). A LIVE settledmark means this request already
// settled — the reserve returns state WITHOUT re-creating a mark (r33c
// residual: a loadgen retry after the mark expired but before the settledmark
// expired re-reserved, double-counting against the same request).
var budgetReserveMarkScript = redis.NewScript(`
local mark = KEYS[1]
local budget = KEYS[2]
local resv = KEYS[3]
local zset = KEYS[4]
local settledmark = KEYS[5]
local e = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttlBudget = tonumber(ARGV[3])
local ttlMark = tonumber(ARGV[4])
local expiry = tonumber(ARGV[5])
local member = ARGV[6]
if redis.call('EXISTS', settledmark) == 1 then
	local cur = tonumber(redis.call('GET', budget)) or 0
	return {cur, limit, 0, 0}
end
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
// budgetset, resvexp, budgetshadow, shadowmark]; ARGV = [cost, markTTLsec,
// scope, targetID, reqID, period, pk, budgetTTLsec]. The reaper-zset member
// is rebuilt INSIDE the script from the mark value (no GET→script race). The
// mark value carries the reservation's period ("e|pk"; legacy plain-float
// marks fall back to the settle's pk): a request that crossed a period
// boundary between admission and settlement must release the reservation
// under the period it was HELD in and charge the ledger row's period —
// releasing and charging one period (old behavior) stranded the new period's
// resv negative forever and froze the old period's headroom until the reaper
// (drift repro T3, 2026-10-21).
//
// Shadow quarantines (r78v, ruling 2026-10-22: a TTL-expired reservation is
// a LEGAL refund but NOT free headroom — the crashed owner may already have
// consumed upstream). The reaper moves an expired hold e from resv into
// budgetshadow WITHOUT touching budget, so admission keeps counting it; the
// shadowmark token ("e|pk", TTL = the settle guard window) makes the later
// clearing exactly-once. A settle that arrives after its hold was reaped
// nets the actual cost against the quarantine (budget += c-e, shadow -= e)
// — the mirror of the live-mark path — and a duplicate settle's release()
// refunds from whichever side still holds e (live resv or shadow).
// Invariant: budget == settled + resv + shadow.
// Returns 2 for a duplicate settle (no-op), 1 settled-against-landed-
// reservation-or-quarantine, 0 reservation-never-landed (full cost).
var budgetSettleScript = redis.NewScript(`
local settledmark = KEYS[1]
local mark = KEYS[2]
local budget = KEYS[3]
local resv = KEYS[4]
local settled = KEYS[5]
local zset = KEYS[6]
local shadow = KEYS[7]
local shadowmark = KEYS[8]
local c = tonumber(ARGV[1])
local ttlMark = tonumber(ARGV[2])
local ttlBudget = tonumber(ARGV[8])
local function parseMark(m)
	local bar = string.find(m, '|', 1, true)
	if bar then
		return tonumber(string.sub(m, 1, bar - 1)), string.sub(m, bar + 1), string.sub(m, 1, bar - 1)
	end
	return tonumber(m), ARGV[7], m
end
-- release() refunds a lingering reservation for this request WITHOUT
-- charging any cost — used on the duplicate path: the cost was already
-- counted by whoever set settledmark (a normal settle, or a far-page
-- certificate), but THIS owner's estimate must not strand (X6: resv has
-- to reconcile with live per-request reserves). With shadow, "lingering"
-- means EITHER a live mark (classic resv refund) OR a reaper quarantine
-- (shadowmark): budget counted the hold all along, so both sides refund
-- budget -= e; exactly-once via the per-token DEL.
local function release()
	local m = redis.call('GET', mark)
	if m then
		local e, mpk, mstr = parseMark(m)
		redis.call('DEL', mark)
		local member = ARGV[3] .. '\31' .. ARGV[4] .. '\31' .. ARGV[5] .. '\31' .. ARGV[6] .. '\31' .. mpk .. '\31' .. mstr
		redis.call('ZREM', zset, member)
		if mpk == ARGV[7] then
			redis.call('INCRBYFLOAT', budget, -e)
			redis.call('INCRBYFLOAT', resv, -e)
		else
			local bOld = 'budget:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
			local rOld = 'budgetresv:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
			redis.call('INCRBYFLOAT', bOld, -e)
			redis.call('INCRBYFLOAT', rOld, -e)
		end
		return
	end
	local sm = redis.call('GET', shadowmark)
	if not sm then
		return
	end
	local e, mpk = parseMark(sm)
	redis.call('DEL', shadowmark)
	if mpk == ARGV[7] then
		redis.call('INCRBYFLOAT', budget, -e)
		redis.call('INCRBYFLOAT', shadow, -e)
	else
		local bOld = 'budget:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
		local shOld = 'budgetshadow:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
		redis.call('INCRBYFLOAT', bOld, -e)
		redis.call('INCRBYFLOAT', shOld, -e)
	end
end
if not redis.call('SET', settledmark, '1', 'NX', 'EX', ttlMark) then
	release()
	return 2
end
redis.call('EXPIRE', settled, ttlBudget)
redis.call('EXPIRE', resv, ttlBudget)
local m = redis.call('GET', mark)
if m then
	local e, mpk, mstr = parseMark(m)
	redis.call('DEL', mark)
	local member = ARGV[3] .. '\31' .. ARGV[4] .. '\31' .. ARGV[5] .. '\31' .. ARGV[6] .. '\31' .. mpk .. '\31' .. mstr
	redis.call('ZREM', zset, member)
	if mpk == ARGV[7] then
		redis.call('INCRBYFLOAT', budget, c - e)
		redis.call('INCRBYFLOAT', resv, -e)
	else
		local bOld = 'budget:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
		local rOld = 'budgetresv:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
		redis.call('INCRBYFLOAT', bOld, -e)
		redis.call('INCRBYFLOAT', rOld, -e)
		redis.call('INCRBYFLOAT', budget, c)
	end
	redis.call('INCRBYFLOAT', settled, c)
	return 1
end
local sm = redis.call('GET', shadowmark)
if sm then
	-- Late settle after the reaper quarantined the expired hold: net the
	-- actual cost against the quarantine (mirror of the live-mark path);
	-- the shadowmark DEL makes the clearing exactly-once even if this
	-- settle replays after a crash mid-window.
	local e, mpk = parseMark(sm)
	redis.call('DEL', shadowmark)
	if mpk == ARGV[7] then
		redis.call('INCRBYFLOAT', budget, c - e)
		redis.call('INCRBYFLOAT', shadow, -e)
	else
		local bOld = 'budget:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
		local shOld = 'budgetshadow:' .. ARGV[3] .. ':' .. ARGV[4] .. ':' .. mpk
		redis.call('INCRBYFLOAT', bOld, -e)
		redis.call('INCRBYFLOAT', shOld, -e)
		redis.call('INCRBYFLOAT', budget, c)
	end
	redis.call('INCRBYFLOAT', settled, c)
	return 1
end
redis.call('INCRBYFLOAT', budget, c)
redis.call('INCRBYFLOAT', settled, c)
return 0
`)

// budgetReapOrphanScript quarantines an EXPIRED reservation (crashed
// request) instead of refunding it (r78v, ruling 2026-10-22): an expired
// hold is a legal TTL refund but NOT free headroom — the crashed owner may
// already have consumed upstream, and nothing can prove it did not. So the
// estimate moves resv -> budgetshadow while budget KEEPS counting it
// (admission unchanged); a shadowmark token ("e|pk", TTL = settle guard
// window) lets a late settle net its actual cost against the quarantine
// exactly once (budgetSettleScript). If no settle ever arrives the amount
// stays counted — the conservative bound for unprovable consumption. The
// ZREM-FIRST claim gives exactly-once semantics ACROSS REPLICAS: with the
// independent reap loop (2026-10-21) two gateways may reap concurrently,
// and "mark absent" alone cannot distinguish not-yet-reaped from
// already-reaped — only the ZREM winner quarantines. A mark still present
// means the score ran ahead of the TTL — caller re-schedules.
// KEYS = [resvmark, budget, budgetresv, resvexp, budgetshadow, shadowmark];
// ARGV = [e, member, shadowAggTTLsec, pk, shadowmarkTTLsec].
// Returns 1 quarantined (and claimed), 0 lost-race-or-still-live.
var budgetReapOrphanScript = redis.NewScript(`
local mark = KEYS[1]
local budget = KEYS[2]
local resv = KEYS[3]
local zset = KEYS[4]
local shadow = KEYS[5]
local shadowmark = KEYS[6]
local e = tonumber(ARGV[1])
local member = ARGV[2]
if redis.call('EXISTS', mark) == 1 then
	return 0
end
if redis.call('ZREM', zset, member) == 0 then
	return 0
end
redis.call('SET', shadowmark, ARGV[1] .. '|' .. ARGV[4], 'NX', 'EX', tonumber(ARGV[5]))
redis.call('INCRBYFLOAT', resv, -e)
redis.call('INCRBYFLOAT', shadow, e)
redis.call('EXPIRE', shadow, tonumber(ARGV[3]))
return 1
`)

func resvMarkKey(scope, targetID, reqID string) string {
	return fmt.Sprintf("resvmark:%s:%s:%s", scope, targetID, reqID)
}

// budgetShadowKey aggregates quarantined expired holds per (scope, target,
// period): admission still counts them inside budget, and the calibration
// raise targets settled + resv + shadow so convergence never converts a
// quarantine back into headroom.
func budgetShadowKey(scope, targetID, pk string) string {
	return fmt.Sprintf("budgetshadow:%s:%s:%s", scope, targetID, pk)
}

// shadowMarkKey is the exactly-once token for clearing one request's
// quarantine (set by the reaper, DEL-ed by the settling owner).
func shadowMarkKey(scope, targetID, reqID string) string {
	return fmt.Sprintf("shadowmark:%s:%s:%s", scope, targetID, reqID)
}

// markTTLSeconds converts a marker TTL to a whole-second floor of at least
// 1s: sub-second test TTLs would otherwise reach the Lua as SET EX 0, which
// Redis rejects outright.
func markTTLSeconds(ttl time.Duration) int {
	secs := int(ttl.Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
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
	journal *settleJournal
	rdb          *redis.Client
	markerTTL    time.Duration
	reapInterval time.Duration
}

func NewBudgetService(rdb *redis.Client) *BudgetService {
	return NewBudgetServiceWithTTLs(rdb, defaultMarkerTTL, DefaultReapInterval)
}

// NewBudgetServiceWithMarkerTTL overrides the reservation/settlement marker
// TTL (config gateway.budget_marker_ttl; tests use short values to exercise
// expiry paths). Markers only outlive a request, so live-mark cardinality
// ≈ in-flight × scopes — the TTL bounds orphan cleanup latency.
func NewBudgetServiceWithMarkerTTL(rdb *redis.Client, ttl time.Duration) *BudgetService {
	if ttl <= 0 {
		ttl = defaultMarkerTTL
	}
	return NewBudgetServiceWithTTLs(rdb, ttl, DefaultReapInterval)
}

// DefaultReapInterval bounds how long an EXPIRED reservation (crashed or
// fault-window request whose settle never landed) can hold admission
// headroom AFTER its mark TTL has passed. The independent reap loop
// (2026-10-21, user-authorized stability work) decouples this detection
// from the hourly calibration tick, cutting the worst-case recovery bound
// from markerTTL+1h to markerTTL+reapInterval. Safety: the loop only ever
// removes marks whose TTL ALREADY expired (the Lua refuses live marks and
// claims exactly-once across replicas via ZREM-first), so shortening the
// INTERVAL never releases a live request, a long stream, or a cross-period
// reservation — only the TTL does that, and it must stay >= max request
// lifetime + replay horizon.
const DefaultReapInterval = 5 * time.Minute

// reapLoopOnce arms the process-wide independent reaper (like the settle
// ledger worker, BudgetService is constructed per calibration round, so the
// loop must be a singleton). Interval <= 0 keeps the legacy behavior: reap
// only inside the hourly calibration round.
var reapLoopOnce sync.Once

func (s *BudgetService) startReapLoop() {
	if s.reapInterval <= 0 {
		return
	}
	reapLoopOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("budget reap loop panicked; calibration tick still reaps hourly", "error", r)
				}
			}()
			ticker := time.NewTicker(s.reapInterval)
			defer ticker.Stop()
			for range ticker.C {
				n := s.ReapExpiredReservations(context.Background(), time.Now())
				if n > 0 {
					slog.Info("budget reap loop removed expired reservations", "count", n)
				}
			}
		}()
	})
}

// newBudgetServiceReapOnly is the transient constructor for one-shot reap
// passes (the calibration round): default markerTTL, NO independent loop —
// the loop must only ever be armed by the configured singleton so a
// disabled reap interval cannot be resurrected by a transient caller.
func newBudgetServiceReapOnly(rdb *redis.Client) *BudgetService {
	return &BudgetService{rdb: rdb, markerTTL: defaultMarkerTTL}
}

// NewBudgetServiceWithTTLs is the full constructor (marker TTL + reap
// interval); wiring comes from config gateway.budget_marker_ttl /
// gateway.budget_reap_interval.
func NewBudgetServiceWithTTLs(rdb *redis.Client, markerTTL, reapInterval time.Duration) *BudgetService {
	s := &BudgetService{rdb: rdb, markerTTL: markerTTL, reapInterval: reapInterval}
	// Re-arm the persistent settle-ledger worker immediately: a restarted
	// process resumes recovering whatever the Redis ledger still holds
	// (r34: crash must not orphan audit-replay tasks).
	s.startSettleLedgerWorker()
	s.startReapLoop()
	return s
}

// ReserveScopeMarked atomically reserves estimate for one scope under a
// request idempotency marker. written=true means the reservation is held
// (created, or already held by an earlier attempt whose response was lost —
// the replay branch reports state without re-incrementing). On Lua failure
// the same definitely-not-sent vs indeterminate classification as v2
// applies, but settlement no longer depends on that bookkeeping: it reads
// the mark as ground truth.
// ErrBudgetEnforcementUnavailable is returned by ReserveForRequestE when
// the ATOMIC reserve could not be executed against the budget store. The
// fail-closed boundary (r78q/r78u reviews): a reserve-script failure means
// admission CANNOT hold an estimate — a plain GET re-check is not atomic
// (concurrent requests all pass it, and nothing deducts the estimate), so
// it is never used as an admission fallback. The request is rejected with
// a TYPED 503 (a dependency outage, NOT a quota verdict — a 429 here
// would misreport enforcement downtime as client-side quota exhaustion).
// A missing counter (redis.Nil at the store level) is NOT an error: the
// reserve script creates the counter, so fresh scopes admit normally.
var ErrBudgetEnforcementUnavailable = errors.New("budget enforcement store unavailable: admission rejected (fail-closed)")

// ReserveScopeMarked is the legacy 4-tuple form (existing callers/tests):
// an enforcement outage reports the scope as exceeded. Production
// admission paths use ReserveForRequestE, which surfaces the typed error.
func (s *BudgetService) ReserveScopeMarked(ctx context.Context, scope, targetID, period, pk, reqID string, estimate, budgetLimit float64) (spent, limit float64, written, exceeded bool) {
	sp, lim, w, x, down := s.reserveScopeMarkedE(ctx, scope, targetID, period, pk, reqID, estimate, budgetLimit)
	if down {
		return sp, lim, false, true
	}
	return sp, lim, w, x
}

func (s *BudgetService) reserveScopeMarkedE(ctx context.Context, scope, targetID, period, pk, reqID string, estimate, budgetLimit float64) (spent, limit float64, written, exceeded, enforcementDown bool) {
	if budgetLimit <= 0 || estimate <= 0 {
		return 0, budgetLimit, false, false, false
	}
	if reqID == "" {
		// No request identity (legacy callers): fall back to the v2 path.
		sp, lim, w, x := s.reserveBudgetKeyed(ctx, scope, targetID, period, pk, estimate, budgetLimit)
		return sp, lim, w, x, false
	}
	bk := fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk)
	rk := budgetResvKey(scope, targetID, pk)
	mk := resvMarkKey(scope, targetID, reqID)
	now := time.Now()
	member := resvExpMember(scope, targetID, reqID, period, pk, fmt.Sprintf("%.9f", estimate))
	res, err := budgetReserveMarkScript.Run(ctx, s.rdb,
		[]string{mk, bk, rk, budgetResvExpZSet, settledMarkKey(scope, targetID, reqID)},
		estimate, budgetLimit,
		int(PeriodTTL(period).Seconds()), markTTLSeconds(s.markerTTL),
		now.Add(s.markerTTL).Unix(), member,
		fmt.Sprintf("%.9f", estimate)+"|"+pk,
	).Float64Slice()
	if err != nil {
		// Fail-closed on ANY atomic-reserve failure: no estimate is held,
		// so admitting (even against a readable GET) is a check-then-act
		// race with no deduction — concurrent over-admission (r78u).
		// Indeterminate errors may have landed the write; the aborted
		// request never settles, so a stray hold expires and the reaper
		// refunds it (bounded by markerTTL).
		if redisWriteDefinitelyNotSent(err) {
			slog.Warn("budget reserve(idempotent) definitely NOT sent; admission fails closed",
				"key", bk, "request_id", reqID, "error", err)
		} else {
			slog.Warn("budget reserve(idempotent) result INDETERMINATE; admission fails closed (stray hold self-heals via marker TTL + reaper)",
				"key", bk, "request_id", reqID, "error", err)
		}
		return 0, budgetLimit, false, false, true
	}
	return res[0], res[1], res[3] == 1, res[2] == 1, false
}

// Settle-retry recovery (r33/r33b finding: a settlement whose whole 30s
// caller window falls inside a longer Redis outage is otherwise lost until
// the hourly DB raise; r34 finding: an in-process-only queue LOSES those
// recovery tasks on process crash and caps out under backlog). Two tiers:
//
//  1. settleLedger — a REDIS-PERSISTENT ZSET ("settleretry"). Enqueued
//     BEFORE giving up (best-effort 5s write; if Redis itself is the outage
//     the write fails and we fall to tier 2). Survives process restarts:
//     every BudgetService construction re-arms the ledger worker, which
//     re-drives due members idempotently (settledmark NX). Metrics expose
//     pending/recovered/expired for operational gating.
//  2. In-process bounded queue (fast path when the ledger write failed —
//     typically the tail of a Redis outage; the ledger worker picks the
//     task up on its next successful ZADD... no: if the ZADD failed, only
//     this queue holds it). Overflow beyond cap is a counted, logged loss
//     in the under-count direction the hourly DB raise heals.
//
// The marker TTL makes a LATE settle exact and safe, so replaying is pure
// recovery. Residual (documented, not hidden): if BOTH the ledger write AND
// the in-process enqueue fail (Redis down + queue full), the settle is lost
// until the hourly DB-based calibration raise — under-count direction,
// alertable via cl_budget_settle_lost_total.
type settleRetryTask struct {
	scope, targetID, period, pk, reqID string
	cost                              float64
	deadline                          time.Time
}

const (
	settleRetryQueueCap   = 65536
	settleRetryHorizon    = 10 * time.Minute
	settleRetryMaxBackoff = 15 * time.Second
	settleLedgerBatch     = 200
	settleLedgerTick      = 2 * time.Second
)

var (
	settleRetryQ    = make(chan settleRetryTask, settleRetryQueueCap)
	settleRetryOnce sync.Once
	settleLedgerOnce sync.Once
)

// Metrics for the settle-recovery tiers (operational gates for r34-class
// verification: pending must drain to 0 after an outage; lost/expired must
// be accounted against the hourly DB raise).
var (
	settleLedgerPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cl_budget_settle_ledger_pending",
		Help: "Settles waiting in the Redis-persistent audit-replay ledger (should return to 0 after an outage).",
	})
	settleLedgerRecoveredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_ledger_recovered_total",
		Help: "Settles recovered by the Redis-persistent audit-replay ledger.",
	})
	settleLedgerExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_ledger_expired_total",
		Help: "Ledger settles past the 10m horizon: under-count direction, healed by the hourly DB-based calibration raise.",
	})
	settleLostTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_lost_total",
		Help: "Settles lost entirely (ledger write failed AND in-process queue full): under-count direction, healed by the hourly DB raise.",
	})
	// r53 forensics (r47 residue, 2026-10-21): 28 deferred settles vanished
	// with NO terminal log (no RECOVERED/EXPIRED/LOST) at the Redis-unpause
	// boundary — 700 of 728 memory-queue entries recovered in one burst at
	// 09:08:10.24 and the rest were never seen again (final ledger=0, marks
	// remained). The exact trigger is UNIDENTIFIED (the vanish left no
	// trace; it was NOT a consumer panic — an unrecovered goroutine panic
	// terminates the whole process and the r47 gateway demonstrably
	// survived). The changes below make the DEFECT CLASS bounded and
	// auditable rather than claiming impossibility: (1) a Tier2 task is
	// upserted to the Redis ledger before its first settle attempt and
	// re-attempted until accepted, so the un-persisted memory-only window
	// shrinks to tasks whose upsert never succeeded; (2) every removal path
	// logs and counts; (3) task conservation — a task the consumer can no
	// longer process is handed off (ledger, else bounded channel) and only
	// counted LOST when both refuse; (4) the channel depth is exported so a
	// stalled consumer is visible. Residual: a memory-only task under a
	// stalled-but-alive consumer is covered only by (4)'s visibility, not by
	// construction.
	settleChannelDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cl_budget_settle_channel_depth",
		Help: "Settles waiting in the in-process audit-replay queue (Tier 2). Non-draining after an outage = stalled consumer; must return to 0.",
	})
	settleConsumerPanicsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_consumer_panics_total",
		Help: "Tier2 settle-consumer panics contained (task handed off via the conservation chain each time).",
	})
	settleLedgerMalformedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_ledger_malformed_total",
		Help: "Malformed ledger entries dropped (audited loudly, never silently).",
	})
)

// settleChannelDepthN backs cl_budget_settle_channel_depth.
var settleChannelDepthN atomic.Int64

// settleTaskProbe is a test seam at the top of every Tier2 task (nil in
// production; same pattern as handler.walRename/walSync) — fault injection
// for the consumer-panic and mid-task-state regressions (T9/T11).
var settleTaskProbe func(t settleRetryTask)

// runSettleScript executes one settle attempt (shared by the inline ladder,
// the in-process drainer and the ledger worker). The settledmark TTL is
// deliberately LONGER than the reservation mark TTL: the settle-once guard
// must outlive the markerTTL + retry horizon so a late replay after the mark
// expired cannot re-settle the same request (r33c residual class).
func (s *BudgetService) runSettleScript(ctx context.Context, t settleRetryTask) (int, error) {
	guardTTL := s.markerTTL + settleRetryHorizon + time.Hour
	return budgetSettleScript.Run(ctx, s.rdb,
		[]string{settledMarkKey(t.scope, t.targetID, t.reqID),
			resvMarkKey(t.scope, t.targetID, t.reqID),
			fmt.Sprintf("budget:%s:%s:%s", t.scope, t.targetID, t.pk),
			budgetResvKey(t.scope, t.targetID, t.pk),
			budgetSettledKey(t.scope, t.targetID, t.pk),
			budgetResvExpZSet,
			budgetShadowKey(t.scope, t.targetID, t.pk),
			shadowMarkKey(t.scope, t.targetID, t.reqID)},
		t.cost, int(guardTTL.Seconds()), t.scope, t.targetID, t.reqID, t.period, t.pk,
		markTTLSeconds(PeriodTTL(t.period)),
	).Int()
}

// ledgerMember encodes a settle task plus its expiry deadline as the ledger
// ZSET member (score = next-attempt epoch).
func ledgerMember(t settleRetryTask) string {
	return t.scope + "\x1f" + t.targetID + "\x1f" + t.period + "\x1f" + t.pk + "\x1f" + t.reqID +
		"\x1f" + strconv.FormatFloat(t.cost, 'f', 9, 64) +
		"\x1f" + strconv.FormatInt(t.deadline.Unix(), 10)
}

func parseLedgerMember(m string) (settleRetryTask, bool) {
	parts := strings.SplitN(m, "\x1f", 7)
	if len(parts) != 7 {
		return settleRetryTask{}, false
	}
	cost, err := strconv.ParseFloat(parts[5], 64)
	if err != nil {
		return settleRetryTask{}, false
	}
	dl, err := strconv.ParseInt(parts[6], 10, 64)
	if err != nil {
		return settleRetryTask{}, false
	}
	return settleRetryTask{
		scope: parts[0], targetID: parts[1], period: parts[2], pk: parts[3],
		reqID: parts[4], cost: cost, deadline: time.Unix(dl, 0),
	}, true
}

func (s *BudgetService) enqueueSettleRetry(t settleRetryTask) bool {
	// Tier 1: Redis-persistent ledger (survives crash/restart). Score = now:
	// due immediately — the worker's own 2s tick is the pacing.
	zctx, zcancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.rdb.ZAdd(zctx, settleLedgerZSet, redis.Z{
		Score:  float64(time.Now().Unix()),
		Member: ledgerMember(t),
	}).Err()
	zcancel()
	if err == nil {
		s.startSettleLedgerWorker()
		return true
	}
	// Tier 2: in-process queue (Redis down — typically the tail of an outage).
	select {
	case settleRetryQ <- t:
		settleChannelDepthN.Add(1)
		settleChannelDepth.Set(float64(settleChannelDepthN.Load()))
		settleRetryOnce.Do(func() { go s.drainSettleRetries() })
		return true
	default:
		settleLostTotal.Inc()
		return false
	}
}

// startSettleLedgerWorker arms the once-per-process ledger drainer. Called
// from every BudgetService construction (a restarted process immediately
// resumes whatever the ledger still holds) and on successful enqueue.
func (s *BudgetService) startSettleLedgerWorker() {
	settleLedgerOnce.Do(func() { go s.drainSettleLedger() })
}

func (s *BudgetService) drainSettleLedger() {
	ticker := time.NewTicker(settleLedgerTick)
	defer ticker.Stop()
	for range ticker.C {
		s.drainSettleLedgerOnce()
	}
}

// drainSettleLedgerOnce drives one due batch. Exported for tests and for an
// operational "drain now" hook.
func (s *BudgetService) drainSettleLedgerOnce() {
	ctx := context.Background()
	if n, err := s.rdb.ZCard(ctx, settleLedgerZSet).Result(); err == nil {
		settleLedgerPending.Set(float64(n))
	}
	for {
		members, err := s.rdb.ZRangeByScore(ctx, settleLedgerZSet, &redis.ZRangeBy{
			Min: "-inf", Max: fmt.Sprintf("%d", time.Now().Unix()), Count: settleLedgerBatch,
		}).Result()
		if err != nil || len(members) == 0 {
			return
		}
		for _, m := range members {
			t, ok := parseLedgerMember(m)
			if !ok {
				// Malformed ledger entry: drop so it cannot wedge the zset —
				// LOUDLY (r53 ①: this was the only unlogged removal path; a
				// drop without a trace is never acceptable for billing).
				settleLedgerMalformedTotal.Inc()
				slog.Warn("budget settle ledger: dropping MALFORMED entry (audited, counted)",
					"member", fmt.Sprintf("%q", m))
				s.rdb.ZRem(ctx, settleLedgerZSet, m)
				continue
			}
			sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
			_, serr := s.runSettleScript(sctx, t)
			scancel()
			if serr == nil {
				s.rdb.ZRem(ctx, settleLedgerZSet, m)
				settleLedgerRecoveredTotal.Inc()
				slog.Info("budget settle ledger RECOVERED",
					"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost)
				continue
			}
			if time.Now().After(t.deadline) {
				s.rdb.ZRem(ctx, settleLedgerZSet, m)
				settleLedgerExpiredTotal.Inc()
				slog.Warn("budget settle ledger EXPIRED past horizon (under-count direction; hourly DB-based calibration raises)",
					"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost, "error", serr)
				continue
			}
			// Re-schedule with backoff (capped).
			s.rdb.ZAdd(ctx, settleLedgerZSet, redis.Z{
				Score:  float64(time.Now().Add(settleRetryMaxBackoff).Unix()),
				Member: m,
			})
		}
		if len(members) < settleLedgerBatch {
			return
		}
	}
}

func (s *BudgetService) drainSettleRetries() {
	for t := range settleRetryQ {
		settleChannelDepthN.Add(-1)
		settleChannelDepth.Set(float64(settleChannelDepthN.Load()))
		s.consumeSettleTask(t)
	}
}

// consumeSettleTask runs one Tier2 task with panic containment and TASK
// CONSERVATION (r58 review hardening). Note on the r47 narrative: an
// UNRECOVERED goroutine panic terminates the whole Go process, and the r47
// gateway process demonstrably survived (same pid start-to-end) — so the
// original vanish was NOT a consumer panic. The containment below is
// defense-in-depth, not a claimed root cause; the conservation contract it
// enforces is: a task this consumer can no longer process is HANDED OFF
// (ledger upsert, else bounded channel re-enqueue) and only counted LOST
// (terminal, loud) when both refuse — never silently dropped.
func (s *BudgetService) consumeSettleTask(t settleRetryTask) {
	defer func() {
		if r := recover(); r != nil {
			settleConsumerPanicsTotal.Inc()
			slog.Error("budget settle audit-replay consumer PANICKED; task handed off, consumer continues",
				"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "panic", r)
			s.handoffTask(t)
		}
	}()
	s.processSettleTask(t)
}

// handoffTask is the conservation handoff for a task the consumer can no
// longer process: Redis-persistent ledger first; if Redis refuses, the
// bounded in-process channel; only when BOTH refuse is the task counted
// LOST (a terminal, loud state — never a silent drop). This is also the
// explicit boundary of the persistence guarantee: a Tier2 task is
// Redis-persistent only from its first SUCCESSFUL ledger upsert (which
// processSettleTask keeps retrying while it settles); tasks whose upsert
// never succeeded remain memory-only and are covered solely by this
// handoff chain and the conservation counters (deferred/terminal counts,
// channel-depth gauge).
func (s *BudgetService) handoffTask(t settleRetryTask) {
	if s.persistLedgerEntry(t) {
		return
	}
	select {
	case settleRetryQ <- t:
		settleChannelDepthN.Add(1)
		settleChannelDepth.Set(float64(settleChannelDepthN.Load()))
		return
	default:
	}
	settleLostTotal.Inc()
	slog.Error("budget settle task LOST at handoff: ledger refused AND channel full (under-count direction; hourly DB-based calibration heals)",
		"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost)
}

// processSettleTask retries one deferred settle until a terminal state.
// Persistence boundary (r58 review): before the FIRST settle attempt the
// task is UPSERTED to the Redis-persistent ledger, and the upsert is
// re-attempted on every retry while it keeps failing — but a task whose
// upsert NEVER succeeded remains memory-only (the loop still settles it;
// conservation of that window is the handoff chain + depth gauge, not a
// persistence guarantee). Once Redis accepts the upsert, a consumer death
// leaves the entry in the ledger where the 2s drain worker retries it to
// the same exactly-once terminal state (the settledmark NX guard makes
// concurrent/dupe attempts no-ops; at worst a task logs one duplicate
// RECOVERED, which is auditable, never silent). The upsert is removed on
// the terminal transition so the ledger does not accumulate settled
// entries.
func (s *BudgetService) processSettleTask(t settleRetryTask) {
	backoff := 2 * time.Second
	persisted := false
	first := true
	for {
		if !persisted {
			persisted = s.persistLedgerEntry(t)
		}
		if first {
			// Probe AFTER the persist attempt, BEFORE the first settle:
			// T11 asserts the task is already Redis-durable here.
			if settleTaskProbe != nil {
				settleTaskProbe(t)
			}
			first = false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := s.runSettleScript(ctx, t)
		cancel()
		if err == nil {
			if persisted {
				s.removeLedgerEntry(t)
			}
			slog.Info("budget settle audit-replay RECOVERED",
				"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost)
			return
		}
		if time.Now().After(t.deadline) {
			if persisted {
				s.removeLedgerEntry(t)
			}
			slog.Warn("budget settle audit-replay EXPIRED (under-count direction; hourly DB-based calibration heals)",
				"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost, "error", err)
			return
		}
		time.Sleep(backoff)
		if backoff < settleRetryMaxBackoff {
			backoff *= 2
		}
	}
}

// persistLedgerEntry upserts a Tier2 (memory-only) task into the Redis
// ledger. Returns true once Redis accepted it.
func (s *BudgetService) persistLedgerEntry(t settleRetryTask) bool {
	zctx, zcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer zcancel()
	return s.rdb.ZAdd(zctx, settleLedgerZSet, redis.Z{
		Score:  float64(time.Now().Unix()),
		Member: ledgerMember(t),
	}).Err() == nil
}

// removeLedgerEntry strips a task's ledger upsert at its terminal state.
func (s *BudgetService) removeLedgerEntry(t settleRetryTask) {
	zctx, zcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer zcancel()
	s.rdb.ZRem(zctx, settleLedgerZSet, ledgerMember(t))
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
	if pk == "" {
		// Defensive (FINDING-B): a settle without a recorded period key
		// (reserve failed in an outage window) must NEVER write to keys
		// with an empty period suffix — those orphan keys are read by no
		// one. Fall back to the current period: the reserve never landed
		// anywhere, so the current period is the exact target.
		pk = PeriodKey(period)
	}
	task := settleRetryTask{
		scope: scope, targetID: targetID, period: period, pk: pk, reqID: reqID, cost: cost,
	}
	// D-R1 crash floor: journal the intent BEFORE the first attempt so a
	// hard kill anywhere in the ladder (or mid attempt-0) leaves a durable
	// record the boot replay settles exactly once. Process-kill durable
	// (append write); power loss remains the calibration layer's residual.
	if s.journal != nil {
		if jerr := s.journal.append(task); jerr != nil {
			settleJournalWriteFailTotal.Inc()
			slog.Warn("settle journal append failed (crash floor degraded for this task)",
				"request_id", reqID, "error", jerr)
		}
	}
	run := func() (int, error) {
		return s.runSettleScript(ctx, task)
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

// ReapExpiredReservations quarantines reservations whose marks expired
// without a settlement (gateway crash mid-request): resv -= e moves into
// budgetshadow and budget KEEPS counting it — an expired hold is a legal
// TTL refund but not free headroom (r78v ruling 2026-10-22: the crashed
// owner may already have consumed upstream). A late settle (WAL replay,
// retry ledger) nets its actual cost against the quarantine exactly once.
// Live marks found by score are re-scheduled. Returns the number of orphans
// quarantined.
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
			scope, targetID, reqID, period, pk, eStr := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
			e, _ := strconv.ParseFloat(eStr, 64)
			guardTTL := s.markerTTL + settleRetryHorizon + time.Hour
			res, err := budgetReapOrphanScript.Run(ctx, s.rdb,
				[]string{resvMarkKey(scope, targetID, reqID),
					fmt.Sprintf("budget:%s:%s:%s", scope, targetID, pk),
					budgetResvKey(scope, targetID, pk),
					budgetResvExpZSet,
					budgetShadowKey(scope, targetID, pk),
					shadowMarkKey(scope, targetID, reqID)},
				e, m,
				markTTLSeconds(PeriodTTL(period)),
				pk,
				int(guardTTL.Seconds()),
			).Int()
			if err != nil {
				slog.Warn("budget reaper: reap failed", "member", m, "error", err)
				continue
			}
			if res == 1 {
				reaped++
				slog.Warn("budget reaper: QUARANTINED expired reservation into shadow (no free headroom; budget still counts it; a late settle nets against the quarantine exactly once)",
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
// ReserveForRequestE is the production admission entry: it returns a
// TYPED error when the atomic reserve could not execute (dependency
// outage — the caller must answer 503 budget_enforcement_unavailable, not
// a quota 429), and exceededScope when a limit genuinely rejected.
func (s *BudgetService) ReserveForRequestE(ctx context.Context, scopes []BudgetScope, estimate float64, reqID string) (*BudgetReservations, string, error) {
	res := &BudgetReservations{
		Reserved:   map[string]float64{},
		PeriodKeys: map[string]string{},
	}
	if estimate <= 0 || len(scopes) == 0 {
		return res, "", nil
	}
	for i, sc := range scopes {
		if sc.Limit <= 0 {
			continue
		}
		// Resolve the period key once, here: reconciliation must adjust the
		// same counter even if the request crosses a UTC period boundary.
		pk := PeriodKey(sc.Period)
		var written, exceeded, down bool
		if reqID != "" {
			_, _, written, exceeded, down = s.reserveScopeMarkedE(ctx, sc.Scope, sc.ID, sc.Period, pk, reqID, estimate, sc.Limit)
		} else {
			_, _, written, exceeded = s.reserveBudgetKeyed(ctx, sc.Scope, sc.ID, sc.Period, pk, estimate, sc.Limit)
		}
		if down {
			// Enforcement outage: refund the levels reserved before this
			// one (identical rollback to a genuine rejection) and reject
			// with the typed error.
			s.refundReserved(ctx, res, scopes, i, reqID)
			return res, sc.Scope, ErrBudgetEnforcementUnavailable
		}
		if exceeded {
			// Refund the levels reserved before this one; the script already
			// rolled back this level's own increment.
			s.refundReserved(ctx, res, scopes, i, reqID)
			return res, sc.Scope, nil
		}
		if written {
			res.Reserved[sc.Scope] = estimate
			res.PeriodKeys[sc.Scope] = pk
		}
	}
	return res, "", nil
}

func (s *BudgetService) refundReserved(ctx context.Context, res *BudgetReservations, scopes []BudgetScope, upto int, reqID string) {
	for j := 0; j < upto; j++ {
		if prev := res.Reserved[scopes[j].Scope]; prev > 0 {
			if reqID != "" {
				s.SettleScope(ctx, scopes[j].Scope, scopes[j].ID, scopes[j].Period, res.PeriodKeys[scopes[j].Scope], reqID, 0)
			} else {
				s.applyBudgetDeltaKeyed(ctx, scopes[j].Scope, scopes[j].ID, scopes[j].Period, res.PeriodKeys[scopes[j].Scope], -prev)
			}
			delete(res.Reserved, scopes[j].Scope)
		}
	}
}

// ReserveForRequest is the legacy 2-tuple form (existing callers/tests):
// an enforcement outage surfaces as the scope being reported exceeded.
func (s *BudgetService) ReserveForRequest(ctx context.Context, scopes []BudgetScope, estimate float64, reqID string) (*BudgetReservations, string) {
	res, scope, err := s.ReserveForRequestE(ctx, scopes, estimate, reqID)
	if err != nil {
		return res, scope
	}
	return res, scope
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
