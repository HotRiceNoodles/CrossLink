package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BudgetCalibrationService struct {
	db  *gorm.DB
	rdb *redis.Client
	mu  sync.Mutex
	// liveIntentCheck reports how many journal intents are still inside
	// the guard window (could be replayed). When >0 the FAR segment is
	// DEFERRED: a live intent's row must go through its settle/replay path
	// only, never the bulk convergence (upgrade-round double-count guard).
	liveIntentCheck func(guard time.Duration) int64
	// journalGuard is the settle-once guard window the JOURNAL uses
	// (markerTTL + horizon + 1h at this deployment). The calibration's
	// near/far boundary MUST be at least this wide: a row older than the
	// journal guard can no longer be replayed (EXPIRED), so absorbing it
	// via the far-segment absolute SUM is double-count-safe; a row inside
	// the window still has a live journal intent and must go through the
	// state-aware settle script ONLY (never the SUM). Injected at assembly
	// from the same markerTTL config; zero = the default formula.
	journalGuard time.Duration
}

// SetLiveIntentCheck installs the journal live-intent probe (same package
// wiring as the guard).
func (s *BudgetCalibrationService) SetLiveIntentCheck(f func(time.Duration) int64) {
	s.liveIntentCheck = f
}

// SetJournalGuard installs the near/far boundary (positive values only).
func (s *BudgetCalibrationService) SetJournalGuard(d time.Duration) {
	if d > 0 {
		s.journalGuard = d
	}
}

func NewBudgetCalibrationService(db *gorm.DB, rdb *redis.Client) *BudgetCalibrationService {
	return &BudgetCalibrationService{db: db, rdb: rdb}
}

// Calibration lock failures are RECOGNIZABLE: a round never proceeds
// unlocked (supervisor review — the previous "proceed on Redis error"
// fallback voided the mutual-exclusion guarantee), and every Redis write
// re-verifies ownership atomically so a lease that expires mid-run stops the
// stale holder instead of letting it corrupt counters.
var (
	ErrCalibrationLockHeld        = errors.New("budget calibration lock held by another replica")
	ErrCalibrationLockUnavailable = errors.New("budget calibration lock store unavailable")
	ErrCalibrationLockLost        = errors.New("budget calibration lock lost mid-run (lease expired)")
)

// calibrationRunBudget bounds one round strictly under the lease TTL so a
// normal round cannot outlive its own lease.
const calibrationRunBudget = 40 * time.Minute

func (s *BudgetCalibrationService) Run(ctx context.Context) {
	interval := time.Hour
	if v := os.Getenv("CL_BUDGET_CALIBRATION_INTERVAL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil && d >= 30*time.Second {
			interval = d
		} else if perr != nil || d < 30*time.Second {
			slog.Warn("budget calibration: invalid CL_BUDGET_CALIBRATION_INTERVAL, using 1h",
				"value", v, "min", "30s")
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.calibrateAll(ctx)
		}
	}
}

// CalibrateOnce (manual/admin trigger) shares the exact same entry as the
// hourly loop: local mutex + cross-replica lock + ownership-checked writes.
// Manual-vs-scheduled concurrency therefore cannot double-calibrate.
func (s *BudgetCalibrationService) CalibrateOnce(ctx context.Context) error {
	return s.calibrateOnceLocked(ctx)
}

func (s *BudgetCalibrationService) calibrateAll(ctx context.Context) {
	if err := s.calibrateOnceLocked(ctx); err != nil {
		switch {
		case errors.Is(err, ErrCalibrationLockHeld):
			slog.Info("budget calibration: another replica holds the lock, skipping this round")
		case errors.Is(err, ErrCalibrationLockUnavailable):
			slog.Warn("budget calibration: lock store unavailable, SKIPPING this round (never unlocked)", "error", err)
		default:
			slog.Warn("budget calibration round failed", "error", err)
		}
		return
	}
	// Positive completion evidence (r78q review: counting log LINES that
	// mention "calibration" is not proof a round ever succeeded).
	slog.Info("budget calibration round completed")
}

func (s *BudgetCalibrationService) calibrateOnceLocked(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, release, err := s.acquireCalibrationLock(ctx)
	if err != nil {
		return err
	}
	if release == nil {
		return ErrCalibrationLockHeld
	}
	defer release()

	runCtx, cancel := context.WithTimeout(ctx, calibrationRunBudget)
	defer cancel()

	// F1′ orphan reaper first: reservations whose marks expired without a
	// settlement (gateway crash mid-request) are removed (budget/resv -= e);
	// their true cost returns via the DB-based raise below from the WAL row.
	if n := newBudgetServiceReapOnly(s.rdb).ReapExpiredReservations(runCtx, time.Now()); n > 0 {
		slog.Warn("budget calibration: reaped expired reservations", "count", n)
	}

	for _, step := range []func(context.Context, string) error{
		s.calibrateTeams, s.calibrateOrgs, s.calibrateKeys,
	} {
		if err := step(runCtx, token); err != nil {
			if errors.Is(err, ErrCalibrationLockLost) {
				// Ownership is gone — stop writing immediately; remaining
				// scopes wait for the (new holder) next round.
				slog.Warn("budget calibration: lease lost mid-run, aborting remaining scopes", "error", err)
				return err
			}
			slog.Warn("budget calibration: scope failed", "error", err)
		}
	}
	return nil
}

const calibrationLockKey = "budget:calibration:lock"
const calibrationLockTTL = 50 * time.Minute

var calibrationUnlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

// acquireCalibrationLock takes the cross-replica lock. Errors are RETURNED,
// never swallowed into an unlocked run: store failures surface as
// ErrCalibrationLockUnavailable so callers skip the round; a taken lock
// surfaces as a nil release. The release is token-guarded so a stale holder
// never deletes a newer holder lock.
func (s *BudgetCalibrationService) acquireCalibrationLock(ctx context.Context) (token string, release func(), err error) {
	var raw [16]byte
	if _, rerr := rand.Read(raw[:]); rerr != nil {
		return "", nil, fmt.Errorf("%w: token: %v", ErrCalibrationLockUnavailable, rerr)
	}
	token = fmt.Sprintf("%x", raw)
	ok, serr := s.rdb.SetNX(ctx, calibrationLockKey, token, calibrationLockTTL).Result()
	if serr != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrCalibrationLockUnavailable, serr)
	}
	if !ok {
		return "", nil, nil
	}
	return token, func() {
		releaseCtx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rcancel()
		if uerr := calibrationUnlockScript.Run(releaseCtx, s.rdb, []string{calibrationLockKey}, token).Err(); uerr != nil {
			slog.Warn("budget calibration: lock release failed (expires by TTL)", "error", uerr)
		}
	}, nil
}

// calibPageProbe is a test seam after each page's atomic apply (nil in
// production): inject mid-round row inserts and Redis faults.
var calibPageProbe func(page int)

// calibAbsSumProbe is a test seam AFTER the ABS-round SUM/MAX ceiling
// query and BEFORE the first proof-setting page (nil in production):
// injects rows committed between two queries that share NO database
// snapshot (X7 — period end is a calendar boundary, not a read point).
var calibAbsSumProbe func()

// calibAbsTxBeginHook is a test seam immediately BEFORE the ABS round
// opens its PostgreSQL snapshot exporter transaction (nil in production):
// injects a BeginTx-class failure to prove the round FAILS CLOSED — no
// proofs, no watermark, never a silent fallback to the falsified plain-
// query path (r78x/X10).
var calibAbsTxBeginHook func() error

// absSnapshotMaxHold bounds the exported-snapshot exporter transaction's
// lifetime (SUM + all far pages): no unbounded long-held transaction.
// Expiry fails the round; the watermark stays untouched and the next
// round retries the target in full.
const absSnapshotMaxHold = 10 * time.Minute

// calibratedWriteScript performs the raise-only convergence of one budget
// counter ATOMICALLY and ONLY while the caller still owns the calibration
// lease: -1 = ownership lost (stale holder must stop), 0 = applied.
//
// v3 review: when the caller passes certification mark keys (KEYS[4..]),
// the SAME invocation that raises the counter sets those settledmarks —
// no instant exists at which a row is counted-but-unmarked (a later
// journal replay would double-apply) or marked-but-uncounted (replay
// would skip an un-billed row). The lock is NOT the crash-atomicity
// mechanism; this single script is.
//
// Raise-only stays a concurrency requirement: normal traffic keeps
// running Reserve/Settle on the same keys uncoordinated, and the page
// snapshot is already stale — the write must never move the counter DOWN.
// The r-aware target (spent + live resv, read INSIDE the script) is the
// r35/r44 drift fix.
var calibratedWriteScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return -1
end
local function raise(key, target, ttl)
	local cur = redis.call('GET', key)
	if not cur or not tonumber(cur) then
		redis.call('SET', key, tostring(target), 'EX', ttl)
		return 1
	end
	local curVal = tonumber(cur)
	if curVal < target then
		redis.call('INCRBYFLOAT', key, target - curVal)
	end
	redis.call('EXPIRE', key, ttl)
	return 0
end
local spent = tonumber(ARGV[2])
local rv = tonumber(redis.call('GET', KEYS[3])) or 0
-- Shadow quarantines (r78v) count in the raise target: converging budget
-- to settled+resv alone would convert a reaper quarantine back into free
-- headroom — the exact defect the quarantine exists to close.
local sh = tonumber(redis.call('GET', KEYS[4])) or 0
local target = spent + rv + sh
local created = raise(KEYS[2], target, ARGV[3])
if sh > 0 then
	redis.call('EXPIRE', KEYS[4], ARGV[3])
end
for i = 5, #KEYS do
	redis.call('SET', KEYS[i], '1', 'EX', tonumber(ARGV[4]))
end
return created`)

// farConvergeScript applies ONE far page. Per row (mark key, cert key,
// cost triplets in KEYS[5..]/ARGV[7..]):
//   - counted-proof = settledmark OR calibcert present. cert is the
//     DURABLE proof (period TTL): once set, no later round ever counts the
//     row again; the mark additionally no-ops a racing LIVE owner settle
//     (X1/R3). Rows with neither proof contribute their cost to this
//     page's increment.
//   - target: "inc" mode -> settled + page increment (normal rounds —
//     additive over an existing near baseline, X4/X5, and idempotent
//     because round N's certs blank round N+1's increments);
//     "abs" mode -> the absolute value in ARGV[6] (B3/upgrade rounds:
//     per-row proofs were evicted with the watermark, so the round-bound
//     SUM is the only truth; its id ceiling excludes mid-round commits,
//     and this round's cert-setting blanks the next round's near pass).
// budget targets settled_target + live resv + shadow quarantines (resv
// reconciles with live per-request reserves; shadow keeps reaper-quarantined
// expired holds counted — without it convergence would reopen headroom,
// r78v). Fixed KEYS = [lock, budget, resv, shadow, settled]; row key
// TRIPLETS [settledmark, calibcert, shadowmark] from KEYS[6]; a certified
// row also nets its shadowmark quarantine (consumption proven). Returns -1
// on lock loss, else rows newly counted.
var farConvergeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return -1
end
local function raise(key, target, ttl)
	local cur = redis.call('GET', key)
	if not cur or not tonumber(cur) then
		redis.call('SET', key, tostring(target), 'EX', ttl)
	else
		local curVal = tonumber(cur)
		if curVal < target then
			redis.call('INCRBYFLOAT', key, target - curVal)
		end
		redis.call('EXPIRE', key, ttl)
	end
end
local inc = 0
local fresh = 0
local n = (#KEYS - 5) / 3
for i = 0, n - 1 do
	local mk = KEYS[6 + 3 * i]
	local ck = KEYS[7 + 3 * i]
	local smk = KEYS[8 + 3 * i]
	local c = tonumber(ARGV[7 + i])
	if redis.call('EXISTS', ck) == 0 and redis.call('EXISTS', mk) == 0 then
		inc = inc + c
		fresh = fresh + 1
	end
	redis.call('SET', ck, '1', 'EX', tonumber(ARGV[4]))
	if redis.call('EXISTS', mk) == 0 then
		redis.call('SET', mk, '1', 'EX', tonumber(ARGV[3]))
	end
	-- Cert netting (r78v): the row PROVES this request consumed. Release
	-- its reaper quarantine (shadowmark DEL = exactly-once) so the proven
	-- cost replaces the quarantined estimate instead of stacking on it.
	-- Skipped when the aggregate already fell below the token (expired
	-- aggregate) to never drive shadow negative.
	local smv = redis.call('GET', smk)
	if smv then
		local bar = string.find(smv, '|', 1, true)
		local e2
		if bar then
			e2 = tonumber(string.sub(smv, 1, bar - 1))
		else
			e2 = tonumber(smv)
		end
		redis.call('DEL', smk)
		local shc = tonumber(redis.call('GET', KEYS[4]))
		if e2 and shc and shc >= e2 - 0.000000001 then
			redis.call('INCRBYFLOAT', KEYS[4], -e2)
			redis.call('INCRBYFLOAT', KEYS[2], -e2)
		end
	end
end
local s0 = tonumber(redis.call('GET', KEYS[5])) or 0
local rv = tonumber(redis.call('GET', KEYS[3])) or 0
local sh = tonumber(redis.call('GET', KEYS[4])) or 0
local st = s0 + inc
if ARGV[5] == 'abs' then
	st = tonumber(ARGV[6])
end
if st > s0 or fresh > 0 then
	raise(KEYS[2], st + rv + sh, ARGV[2])
	raise(KEYS[5], st, ARGV[2])
	if sh > 0 then
		redis.call('EXPIRE', KEYS[4], ARGV[2])
	end
end
return fresh`)

// fetchSubstituteDollar rewrites gorm-style ? placeholders into PostgreSQL
// $N ordinals for the raw snapshot-bounded far-page query.
func fetchSubstituteDollar(q string) string {
	n := 0
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
		} else {
			b.WriteByte(q[i])
		}
	}
	return b.String()
}

// calibrateTarget calibrates ONE (scope,id,pk) with a consistent per-page
// snapshot and crash-atomic apply (v3 review):
//   - pages of rows (keyset by id, 5000/page, NO total limit) come from ONE
//     query per page — the page's rows and its sum are the same snapshot,
//     so a row can never be marked without being counted;
//   - each page is applied by ONE script invocation [raise-to-cumulative +
//     certify the page's rows] — atomic w.r.t. crashes and concurrent
//     writers;
//   - the budgetaudit watermark advances ONLY after every page succeeded;
//     any query/script failure aborts the target with the watermark
//     untouched, and the next round re-pages idempotently (raise-only +
//     SET marks are idempotent).
// Rows committed mid-round with an id behind the cursor are neither counted
// nor marked this round (they are owned by the normal settle path or the
// journal replay — never silently absorbed).
func (s *BudgetCalibrationService) calibrateTarget(ctx context.Context, token, scope string, id int64, pk, period string, start, end time.Time) (float64, error) {
	idCol := map[string]string{"team": "team_id", "org": "org_id", "key": "api_key_id"}[scope]
	if idCol == "" {
		return 0, fmt.Errorf("calibrateTarget: unknown scope %q", scope)
	}
	guard := s.journalGuard
	if guard <= 0 {
		guard = defaultMarkerTTL + 10*time.Minute + time.Hour
	}
	tid := fmt.Sprintf("%d", id)
	budgetKey := fmt.Sprintf("budget:%s:%d:%s", scope, id, pk)
	resvKey := budgetResvKey(scope, tid, pk)
	ttl := int(PeriodTTL(period).Seconds())
	wmKey := fmt.Sprintf("budgetaudit:%s:%d:%s", scope, id, pk)

	// Watermark v3: "v3|spent|cursor|guardID".
	//   cursor   — near-segment id cursor (rows above it are re-examined);
	//   guardID  — the id horizon of the (now-guard-skew) boundary: rows
	//              ABOVE it may still carry a LIVE journal intent on ANY
	//              instance (they are near: state-aware settle script
	//              only); rows at or below it cannot — the journal's own
	//              EXPIRED guard guarantees no replay for them anywhere,
	//              so bulk raise-only absorption can never double-count
	//              (cross-replica review X1: the local journal cannot
	//              prove the absence of a FOREIGN live intent, so the
	//              boundary must come from the guard window, never from
	//              MAX(id) at round start).
	// guardID is RECOMPUTED every round: created_at is DB-assigned at
	// insert (id order == created_at order), so MAX(id | created_at <
	// horizon) ages rows into FAR monotonically, keeps NEAR bounded by
	// the guard window, and replaces the frozen first-round value (which
	// made NEAR re-walk the whole period every round — X3). The +10m
	// skew margin covers cross-replica clock drift (the owner ages its
	// journal line by ITS clock); drift beyond it must be corrected by
	// NTP.
	// A v2 ("v2|spent|id") or legacy ("spent|unix") watermark carries no
	// trustworthy cursor: near resets (cursor=0) — one fully-idempotent
	// re-scan, safest direction.
	cursor := int64(0)
	wmSeen, wmIsV3 := false, false
	if wm, err := s.rdb.Get(ctx, wmKey).Result(); err == nil {
		wmSeen = true
		parts := strings.Split(wm, "|")
		if len(parts) == 4 && parts[0] == "v3" {
			wmIsV3 = true
			if v, e1 := strconv.ParseInt(parts[2], 10, 64); e1 == nil {
				cursor = v
			}
		}
	} else if err != redis.Nil {
		return 0, fmt.Errorf("watermark read: %w", err)
	}
	if wmSeen && !wmIsV3 {
		// v2/legacy watermark: its second field is NOT a near cursor —
		// reset for one fully-idempotent re-scan.
		cursor = 0
	}
	guardID := int64(0)
	b3Class := false
	if !wmIsV3 {
		// Evidence of prior settlement without a trustworthy v3 cursor
		// (vanished watermark, or a legacy/v2 one): rows may have been
		// settled by marks that have since EXPIRED or been EVICTED, so
		// per-row near re-claims could double-count (B3). Everything
		// pre-existing goes FAR — one conservative raise+certify pass.
		// budgetset is the evidence key: reserves never write it, every
		// settle/convergence does.
		b3Class = s.rdb.Exists(ctx, budgetSettledKey(scope, tid, pk)).Val() == 1
	}
	if b3Class {
		qc, qcc := context.WithTimeout(ctx, 30*time.Second)
		var mr struct{ MaxID int64 }
		if gerr := s.db.WithContext(qc).Model(&model.UsageLog{}).
			Select("COALESCE(MAX(id), 0) as max_id").
			Where(idCol+" = ? AND created_at >= ? AND created_at < ?", id, start, end).
			Scan(&mr).Error; gerr != nil {
			qcc()
			return 0, fmt.Errorf("guard id: %w", gerr)
		}
		qcc()
		guardID = mr.MaxID
	} else {
		// Steady v3 regime OR a fresh deployment (no watermark, no
		// settlement evidence): the time-based near/far horizon (see the
		// watermark comment above) — rows older than (guard + skew)
		// cannot carry a live intent on any instance, so MAX(id) below
		// that created_at is the FAR boundary. Recomputed every round so
		// rows age into FAR monotonically and NEAR stays bounded by the
		// guard window (the stored guardID used to be frozen at its first
		// value, making NEAR re-walk the whole period every round — X3).
		// The +10m skew margin covers cross-replica clock drift (the
		// owner ages its journal line by ITS clock); drift beyond it must
		// be corrected by NTP.
		horizon := time.Now().Add(-(guard + 10*time.Minute))
		hc, hcc := context.WithTimeout(ctx, 10*time.Second)
		var hr struct{ MaxID int64 }
		if herr := s.db.WithContext(hc).Model(&model.UsageLog{}).
			Select("COALESCE(MAX(id), 0) as max_id").
			Where(idCol+" = ? AND created_at >= ? AND created_at < ?", id, start, horizon).
			Scan(&hr).Error; herr != nil {
			hcc()
			return 0, fmt.Errorf("guard id horizon: %w", herr)
		}
		hcc()
		guardID = hr.MaxID
	}

	bs := &BudgetService{rdb: s.rdb, markerTTL: defaultMarkerTTL}
	// Whole-target deferral while THIS instance's journal still holds live
	// intents (B1): the far certificates make absorption exactly-once even
	// against a live owner, but deferring keeps the intent's own
	// settle/replay path primary — and under-counting until the intents
	// drain (minutes) is the conservative direction. No watermark is
	// written; the next round retries the target in full.
	if s.liveIntentCheck != nil && s.liveIntentCheck(guard) > 0 {
		slog.Warn("budget calibration: target deferred (journal holds live intents)",
			"scope", scope, "id", id)
		spent, _ := s.rdb.Get(ctx, budgetSettledKey(scope, tid, pk)).Float64()
		return spent, nil
	}
	lastID := cursor
	// NEAR: id-bearing rows above guardID (live-journal domain), each via
	// the SAME state-aware exactly-once script (review 1: estimates are
	// released, in-flight originals no-op on the mark). A period-length
	// calibration certificate makes re-scans cheap and marks the row as
	// converged for the whole period regardless of the settle guard TTL.
	for page := 0; ; page++ {
		type row struct {
			ID        int64
			RequestID string
			Cost      float64
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var rows []row
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("id, request_id, cost").
			Where(idCol+" = ? AND cost > 0 AND request_id <> '' AND created_at >= ? AND created_at < ? AND id > ? AND id > ?", id, start, end, lastID, guardID).
			Order("id").Limit(5000).Find(&rows).Error
		qCancel()
		if err != nil {
			return 0, fmt.Errorf("near page query: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		if calibPageProbe != nil {
			calibPageProbe(page)
		}
		// Ownership re-verify per page (the row-level settle script does
		// not itself check the calibration lock): a stale holder must be
		// rejected BEFORE applying anything.
		if ok, verr := lockRenewScript.Run(ctx, s.rdb,
			[]string{calibrationLockKey}, token,
			int64((calibrationLockTTL - 5*time.Minute).Milliseconds())).Int(); verr != nil || ok != 1 {
			return 0, fmt.Errorf("%w: key=%s (near page verify)", ErrCalibrationLockLost, budgetKey)
		}
		pipe := s.rdb.Pipeline()
		certCmds := make([]*redis.IntCmd, 0, len(rows))
		certKeys := make([]string, 0, len(rows))
		for _, r := range rows {
			k := fmt.Sprintf("calibcert:%s:%s:%s", scope, tid, r.RequestID)
			certKeys = append(certKeys, k)
			certCmds = append(certCmds, pipe.Exists(ctx, k))
		}
		if _, perr := pipe.Exec(ctx); perr != nil && !errors.Is(perr, redis.Nil) {
			return 0, fmt.Errorf("near cert pre-check: %w", perr)
		}
		for i, r := range rows {
			if certCmds[i].Err() != nil {
				return 0, fmt.Errorf("near cert read %s: %w", r.RequestID, certCmds[i].Err())
			}
			if certCmds[i].Val() == 1 {
				continue // converged earlier this period
			}
			tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
			_, serr := bs.runSettleScript(tctx, settleRetryTask{
				scope: scope, targetID: tid, period: period, pk: pk,
				reqID: r.RequestID, cost: r.Cost,
			})
			tcancel()
			if serr != nil {
				return 0, fmt.Errorf("near row apply %s: %w", r.RequestID, serr)
			}
			if cerr := s.rdb.Set(ctx, certKeys[i], "1", PeriodTTL(period)).Err(); cerr != nil {
				// Abort WITHOUT advancing the watermark: the settledmark
				// (live ~guard window) still shields the row on the retry;
				// only a cert-write outage longer than that guard could
				// re-open it (documented residual, B2).
				return 0, fmt.Errorf("cert write %s: %w", r.RequestID, cerr)
			}
		}
		lastID = rows[len(rows)-1].ID
		if len(rows) < 5000 {
			break
		}
	}
	// FAR: paged convergence under the unified proof-of-counted rule.
	// Normal rounds count each unproven row ONCE (calibcert = durable
	// period proof; settledmark no-ops a racing live owner, X1/R3); the
	// increment is additive over the existing settled baseline (X4/X5)
	// and idempotent because this round's certs blank the next round's
	// increments. The upgrade class (b3Class: per-row proofs were evicted
	// together with the watermark) raises ABSOLUTELY to the SUM of rows
	// at/below a PAIRED SUM+MAX ceiling captured in ONE query. That
	// ceiling is a round-start id bound, NOT a database snapshot — period
	// end is a calendar boundary and the SUM and the per-page Find below
	// are independent queries. Rows committed after the ceiling query
	// carry HIGHER global ids, so the ABS pages bound themselves to the
	// ceiling (dropping the request_id='' catch-all, X7): a mid-round
	// commit is never proofed ahead of its cost — the next NORMAL round
	// counts it. Rows inside the ceiling get proofs here, blanking the
	// next round's near pass (B3); a foreign YOUNG row absorbed by the
	// raise is marked so its delayed owner settle no-ops and releases its
	// reservation (X8).
	farMode := "inc"
	absSum := 0.0
	absCeil := int64(-1)
	// absSnap (PostgreSQL only) is the exported REPEATABLE READ snapshot
	// the ABS SUM ran under: r78w/X9 — the paired SUM+MAX bounds IDs but
	// is NOT a snapshot. A row whose transaction INSERTed early (sequence
	// allocates the id BEFORE commit) and commits AFTER the SUM is
	// invisible to the sum yet visible to each page query's own READ
	// COMMITTED snapshot: it enters the page below the ceiling, gets
	// proofed, and its cost never lands — permanent under-count. Under one
	// exported snapshot the pages see EXACTLY the SUM's row set; the
	// late-commit row is left to the next round, which counts and certifies
	// it properly (it is then committed and its id is below the new
	// horizon for the far-inc segment).
	//
	// r78x hardening (review 2026-10-23 round 13): on PostgreSQL a
	// Begin/sum/export failure must FAIL THE ROUND (no proofs, no
	// watermark — the falsified plain-query path stays unreachable in
	// production); the paired-query fallback below is gated to NON-PG
	// drivers ONLY (unit-test sqlite serializes writers; the window cannot
	// interleave there). The exporter tx has a BOUNDED lifetime
	// (absSnapshotMaxHold): no unbounded long-held transaction — expiry
	// kills the round, the watermark stays untouched, the next round
	// retries in full.
	absSnap := ""
	var snapTx *sql.Tx
	var snapCtx context.Context
	var snapCancel context.CancelFunc
	if b3Class {
		farMode = "abs"
		if s.db.Dialector.Name() == "postgres" {
			sqlDB, derr := s.db.DB()
			if derr != nil {
				return 0, fmt.Errorf("abs round requires the PG snapshot (pool: %w)", derr)
			}
			if calibAbsTxBeginHook != nil {
				if herr := calibAbsTxBeginHook(); herr != nil {
					return 0, fmt.Errorf("abs round requires the PG snapshot (begin hook: %w)", herr)
				}
			}
			snapCtx, snapCancel = context.WithTimeout(ctx, absSnapshotMaxHold)
			defer snapCancel()
			tx, terr := sqlDB.BeginTx(snapCtx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
			if terr != nil {
				return 0, fmt.Errorf("abs round requires the PG snapshot (begin: %w)", terr)
			}
			var spentF float64
			var ceil int64
			qerr := tx.QueryRowContext(snapCtx,
				"SELECT COALESCE(SUM(cost),0), COALESCE(MAX(id),0) FROM usage_logs WHERE "+idCol+
					" = $1 AND cost > 0 AND created_at >= $2 AND created_at < $3 AND id <= $4",
				id, start, end, guardID).Scan(&spentF, &ceil)
			if qerr != nil {
				_ = tx.Rollback()
				return 0, fmt.Errorf("abs round snapshot-bounded sum: %w", qerr)
			}
			var snap string
			if serr := tx.QueryRowContext(snapCtx, "SELECT pg_export_snapshot()").Scan(&snap); serr != nil {
				_ = tx.Rollback()
				return 0, fmt.Errorf("abs round requires the PG snapshot (export: %w)", serr)
			}
			absSum, absCeil, absSnap, snapTx = spentF, ceil, snap, tx
			defer func() { _ = snapTx.Rollback() }() // read-only exporter; snapshot dies with it
		} else {
			// NON-PG driver ONLY (unit-test sqlite: single serialized
			// writer — the commit-after-SUM window cannot interleave).
			// Production is PostgreSQL and never reaches this branch.
			asq, asqc := context.WithTimeout(ctx, 30*time.Second)
			var asr struct {
				Spent float64
				MaxID int64
			}
			if aerr := s.db.WithContext(asq).Model(&model.UsageLog{}).
				Select("COALESCE(SUM(cost), 0) as spent, COALESCE(MAX(id), 0) as max_id").
				Where(idCol+" = ? AND cost > 0 AND created_at >= ? AND created_at < ? AND id <= ?", id, start, end, guardID).
				Scan(&asr).Error; aerr != nil {
				asqc()
				return 0, fmt.Errorf("far abs sum: %w", aerr)
			}
			asqc()
			absSum = asr.Spent
			absCeil = asr.MaxID
		}
		if calibAbsSumProbe != nil {
			calibAbsSumProbe()
		}
	}
	{
		farCursor := int64(0)
		for page := 0; ; page++ {
			type frow struct {
				ID        int64
				RequestID string
				Cost      float64
			}
			// Snapshot pages derive from the BOUNDED exporter ctx: if the
			// exporter's lifetime expires mid-paging, the page fails and
			// the round aborts (watermark untouched) — never an unbounded
			// hold, never a fallback path.
			pageCtx := ctx
			if absSnap != "" && snapCtx != nil {
				pageCtx = snapCtx
			}
			fq, fqc := context.WithTimeout(pageCtx, 30*time.Second)
			var frows []frow
			fetchWhere := idCol + " = ? AND cost > 0 AND (request_id = '' OR id <= ?) AND created_at >= ? AND created_at < ? AND id > ?"
			fetchArgs := []interface{}{id, guardID, start, end, farCursor}
			if farMode == "abs" {
				// Ceiling-bounded: rows above the PAIRED SUM+MAX bound are
				// NOT in the absolute target — never proof them here (a
				// request_id='' row committed mid-round at a higher id
				// would be marked without its cost, X7).
				fetchWhere = idCol + " = ? AND cost > 0 AND id <= ? AND created_at >= ? AND created_at < ? AND id > ?"
				fetchArgs = []interface{}{id, absCeil, start, end, farCursor}
			}
			if absSnap != "" {
				// X9: run the page under the SAME exported snapshot the ABS
				// SUM used — id ceiling AND visibility bounded by the SUM's
				// read point; a late-committing low-id row stays invisible
				// here and is owned by the next round.
				sqlDB, derr := s.db.DB()
				if derr != nil {
					fqc()
					return 0, fmt.Errorf("far page conn: %w", derr)
				}
				ptx, terr := sqlDB.BeginTx(fq, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
				if terr != nil {
					fqc()
					return 0, fmt.Errorf("far page tx: %w", terr)
				}
				if _, e := ptx.ExecContext(fq, "SET TRANSACTION SNAPSHOT '"+
					strings.ReplaceAll(absSnap, "'", "''")+"'"); e != nil {
					ptx.Rollback()
					fqc()
					return 0, fmt.Errorf("far page snapshot: %w", e)
				}
				rowsQ, qerr := ptx.QueryContext(fq,
					"SELECT id, request_id, cost FROM usage_logs WHERE "+fetchSubstituteDollar(fetchWhere)+" ORDER BY id LIMIT 5000", id, absCeil, start, end, farCursor)
				if qerr != nil {
					ptx.Rollback()
					fqc()
					return 0, fmt.Errorf("far page query: %w", qerr)
				}
				for rowsQ.Next() {
					var r frow
					if e := rowsQ.Scan(&r.ID, &r.RequestID, &r.Cost); e != nil {
						rowsQ.Close()
						ptx.Rollback()
						fqc()
						return 0, fmt.Errorf("far page scan: %w", e)
					}
					frows = append(frows, r)
				}
				rowsQ.Close()
				ptx.Rollback()
				fqc()
			} else {
				ferr := s.db.WithContext(fq).Model(&model.UsageLog{}).
					Select("id, request_id, cost").
					Where(fetchWhere, fetchArgs...).
					Order("id").Limit(5000).Find(&frows).Error
				fqc()
				if ferr != nil {
					return 0, fmt.Errorf("far page query: %w", ferr)
				}
			}
			if len(frows) == 0 {
				break
			}
			// Ownership re-verify per page, same as near.
			if ok, verr := lockRenewScript.Run(ctx, s.rdb,
				[]string{calibrationLockKey}, token,
				int64((calibrationLockTTL - 5*time.Minute).Milliseconds())).Int(); verr != nil || ok != 1 {
				return 0, fmt.Errorf("%w: key=%s (far page verify)", ErrCalibrationLockLost, budgetKey)
			}
			keys := make([]string, 0, 3*len(frows)+5)
			keys = append(keys, calibrationLockKey, budgetKey, resvKey,
				budgetShadowKey(scope, tid, pk), budgetSettledKey(scope, tid, pk))
			argv := make([]interface{}, 0, len(frows)+6)
			argv = append(argv, token, int(ttl), int((guard+time.Hour).Seconds()),
				int(PeriodTTL(period).Seconds()), farMode, absSum)
			for _, r := range frows {
				if r.RequestID != "" {
					keys = append(keys,
						settledMarkKey(scope, tid, r.RequestID),
						fmt.Sprintf("calibcert:%s:%s:%s", scope, tid, r.RequestID),
						shadowMarkKey(scope, tid, r.RequestID))
				} else {
					// Legacy/admin row: no request-scoped keys exist; a
					// row-id pair provides the same durable proof.
					keys = append(keys,
						settledMarkKey(scope, tid, fmt.Sprintf("row-%d", r.ID)),
						fmt.Sprintf("calibcert:%s:%s:row-%d", scope, tid, r.ID),
						shadowMarkKey(scope, tid, fmt.Sprintf("row-%d", r.ID)))
				}
				argv = append(argv, r.Cost)
			}
			res, aerr := farConvergeScript.Run(ctx, s.rdb, keys, argv...).Int()
			if aerr != nil {
				return 0, fmt.Errorf("far convergence apply: %w", aerr)
			}
			if res == -1 {
				return 0, fmt.Errorf("%w: key=%s", ErrCalibrationLockLost, budgetKey)
			}
			farCursor = frows[len(frows)-1].ID
			if len(frows) < 5000 {
				break
			}
		}
	}
	spent, _ := s.rdb.Get(ctx, budgetSettledKey(scope, tid, pk)).Float64()
	s.rdb.Set(ctx, wmKey, fmt.Sprintf("v3|%.9f|%d|%d", spent, lastID, guardID), PeriodTTL(period))
	return spent, nil
}

// finalizeTarget writes the post-raise bookkeeping (F1' invariant check,
// watermark, PG snapshot). Called only after calibrateTarget succeeded —
// the watermark advancing past a failed round is the v3-review bug it

// calibrateTargetSync is the row-less form of one target's calibration
// (atomic 3-key raise + finalize). Production paths use calibrateTarget
// (paged rows + certification); the legacy drift/marked/ledger
// regressions drive counters directly and keep this exact entry.
func (s *BudgetCalibrationService) calibrateTargetSync(ctx context.Context, token, scope string, id int64, pk, period string, spent, budget float64) error {
	budgetKey := fmt.Sprintf("budget:%s:%d:%s", scope, id, pk)
	resvKey := budgetResvKey(scope, fmt.Sprintf("%d", id), pk)
	res, err := calibratedWriteScript.Run(ctx, s.rdb,
		[]string{calibrationLockKey, budgetKey, resvKey, budgetShadowKey(scope, fmt.Sprintf("%d", id), pk)}, token, spent,
		int(PeriodTTL(period).Seconds()),
		int((defaultMarkerTTL + 10*time.Minute + time.Hour).Seconds())).Int()
	if err != nil {
		return err
	}
	if res == -1 {
		return fmt.Errorf("%w: key=%s", ErrCalibrationLockLost, budgetKey)
	}
	s.finalizeTarget(ctx, scope, id, pk, period, spent, budget)
	return nil
}
// prevents.
func (s *BudgetCalibrationService) finalizeTarget(ctx context.Context, scope string, id int64, pk, period string, spent, budget float64) {
	// The budgetaudit watermark (spent|id-cursor) is written by
	// calibrateTarget's success tail — never here, never on failure.
	_ = s.migrateOrCheckInvariant(ctx, scope, id, pk, period, spent)
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "target_type"}, {Name: "target_id"}, {Name: "period_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"spent", "budget"}),
	}).Create(&model.BudgetSnapshot{
		TargetType: scope, TargetID: id, PeriodKey: pk, Spent: spent, Budget: budget,
	}).Error; err != nil {
		slog.Warn("budget calibration: failed to save snapshot", "scope", scope, "id", id, "error", err)
	}
}

func (s *BudgetCalibrationService) calibrateTeams(ctx context.Context, token string) error {
	var teams []model.Team
	if err := s.db.WithContext(ctx).Where("budget_limit > 0").Find(&teams).Error; err != nil {
		return fmt.Errorf("query teams: %w", err)
	}
	if len(teams) == 0 {
		return nil
	}

	// Single GROUP BY query instead of N+1
	type spendRow struct {
		TeamID int64
		Spent  float64
	}
	// Aggregate per-period: group teams by period, then one query per period
	byPeriod := make(map[string][]model.Team)
	for _, t := range teams {
		pk := PeriodKey(t.BudgetPeriod)
		byPeriod[pk+"|"+t.BudgetPeriod] = append(byPeriod[pk+"|"+t.BudgetPeriod], t)
	}
	for key, periodTeams := range byPeriod {
		parts := strings.SplitN(key, "|", 2)
		pk, period := parts[0], parts[1]
		start, end := periodBoundaries(period)
		ids := make([]int64, len(periodTeams))
		for i, t := range periodTeams {
			ids[i] = t.ID
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var periodRows []spendRow
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("team_id, COALESCE(SUM(cost), 0) as spent").
			Where("team_id IN ? AND created_at >= ? AND created_at < ?", ids, start, end).
			Group("team_id").
			Scan(&periodRows).Error
		qCancel()
		if err != nil {
			slog.Warn("budget calibration: failed to aggregate team spend", "period", period, "error", err)
			continue
		}
		spendMap := make(map[int64]float64, len(periodRows))
		for _, r := range periodRows {
			spendMap[r.TeamID] = r.Spent
		}
		_ = spendMap
		for _, team := range periodTeams {
			spent, cerr := s.calibrateTarget(ctx, token, "team", team.ID, pk, period, start, end)
			if cerr != nil {
				if errors.Is(cerr, ErrCalibrationLockLost) {
					return cerr
				}
				slog.Warn("budget calibration: target failed (watermark NOT advanced; retried next round)",
					"scope", "team", "id", team.ID, "error", cerr)
				continue
			}
			s.finalizeTarget(ctx, "team", team.ID, pk, period, spent, team.BudgetLimit)
		}
	}
	return nil
}

func (s *BudgetCalibrationService) calibrateOrgs(ctx context.Context, token string) error {
	var orgs []model.Organization
	if err := s.db.WithContext(ctx).Where("budget_limit > 0 AND deleted_at IS NULL").Find(&orgs).Error; err != nil {
		return fmt.Errorf("query orgs: %w", err)
	}
	if len(orgs) == 0 {
		return nil
	}

	type spendRow struct {
		OrgID int64 `gorm:"column:org_id"`
		Spent float64
	}
	byPeriod := make(map[string][]model.Organization)
	for _, o := range orgs {
		pk := PeriodKey(o.BudgetPeriod)
		byPeriod[pk+"|"+o.BudgetPeriod] = append(byPeriod[pk+"|"+o.BudgetPeriod], o)
	}
	for key, periodOrgs := range byPeriod {
		parts := strings.SplitN(key, "|", 2)
		pk, period := parts[0], parts[1]
		start, end := periodBoundaries(period)
		ids := make([]int64, len(periodOrgs))
		for i, o := range periodOrgs {
			ids[i] = o.ID
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var periodRows []spendRow
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("org_id, COALESCE(SUM(cost), 0) as spent").
			Where("org_id IN ? AND created_at >= ? AND created_at < ?", ids, start, end).
			Group("org_id").
			Scan(&periodRows).Error
		qCancel()
		if err != nil {
			slog.Warn("budget calibration: failed to aggregate org spend", "period", period, "error", err)
			continue
		}
		for _, org := range periodOrgs {
			spent, cerr := s.calibrateTarget(ctx, token, "org", org.ID, pk, period, start, end)
			if cerr != nil {
				if errors.Is(cerr, ErrCalibrationLockLost) {
					return cerr
				}
				slog.Warn("budget calibration: target failed (watermark NOT advanced; retried next round)",
					"scope", "org", "id", org.ID, "error", cerr)
				continue
			}
			s.finalizeTarget(ctx, "org", org.ID, pk, period, spent, org.BudgetLimit)
		}
	}
	return nil
}

func (s *BudgetCalibrationService) calibrateKeys(ctx context.Context, token string) error {
	var keys []model.APIKey
	if err := s.db.WithContext(ctx).Where("max_budget > 0").Find(&keys).Error; err != nil {
		return fmt.Errorf("query keys: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}

	type spendRow struct {
		APIKeyID int64 `gorm:"column:api_key_id"`
		Spent    float64
	}
	byPeriod := make(map[string][]model.APIKey)
	for _, k := range keys {
		pk := PeriodKey(k.BudgetPeriod)
		byPeriod[pk+"|"+k.BudgetPeriod] = append(byPeriod[pk+"|"+k.BudgetPeriod], k)
	}
	for key, periodKeys := range byPeriod {
		parts := strings.SplitN(key, "|", 2)
		pk, period := parts[0], parts[1]
		start, end := periodBoundaries(period)
		ids := make([]int64, len(periodKeys))
		for i, k := range periodKeys {
			ids[i] = k.ID
		}
		qCtx, qCancel := context.WithTimeout(ctx, 30*time.Second)
		var periodRows []spendRow
		err := s.db.WithContext(qCtx).Model(&model.UsageLog{}).
			Select("api_key_id, COALESCE(SUM(cost), 0) as spent").
			Where("api_key_id IN ? AND created_at >= ? AND created_at < ?", ids, start, end).
			Group("api_key_id").
			Scan(&periodRows).Error
		qCancel()
		if err != nil {
			slog.Warn("budget calibration: failed to aggregate key spend", "period", period, "error", err)
			continue
		}
		for _, k := range periodKeys {
			spent, cerr := s.calibrateTarget(ctx, token, "key", k.ID, pk, period, start, end)
			if cerr != nil {
				if errors.Is(cerr, ErrCalibrationLockLost) {
					return cerr
				}
				slog.Warn("budget calibration: target failed (watermark NOT advanced; retried next round)",
					"scope", "key", "id", k.ID, "error", cerr)
				continue
			}
			s.finalizeTarget(ctx, "key", k.ID, pk, period, spent, k.MaxBudget)
		}
	}
	return nil
}

// calibratedWriteScript performs the raise-only convergence of one budget
// counter ATOMICALLY and ONLY while the caller still owns the calibration
// lease: -1 = ownership lost (stale holder must stop), 1 = key created,
// 0 = reconciled in place. This also removes the old GET→delta→INCRBYFLOAT
// interleave two replicas could double-apply.
//
// Raise-only is a concurrency requirement, not a stylistic choice: the
// calibration lock only mutual-excludes CALIBRATORS. Normal traffic keeps
// running Reserve (pre-request) and Adjust (settlement) on the same keys
// with no coordination, and `spent` is a DB snapshot that is already stale
// by the time this runs — so the write must never move the counter DOWN:
//   - counter ABOVE the snapshot = in-flight reservations plus consumption
//     newer than the snapshot. Lowering erases live reservations; their
//     settlements then apply (cost-reserved) against a counter missing the
//     reserve, under-counting by the reserve amount (the review's
//     "in-flight wipe" hazard — every hourly round under traffic would do it).
//   - counter BELOW the snapshot = under-count (outage fail-open residue);
//     raising converges it to ledger truth.
// Known boundary: when the counter is below the snapshot WHILE a
// reservation is in flight, the raise absorbs that reservation and its
// settlement lands (cost-reserved) net-short by the reserve amount. That
// residue is bounded by the in-flight reserves at write time and is closed
// by the NEXT hourly round (the request has settled; the fresh SUM covers
// it). The counter is derived data; the ledger (usage_logs) is untouched.
//
// Drift fix (r35/r44, 2026-10-21, repro T1/T2): the raise target must
// include LIVE reservations — raising budget to bare SUM(ledger) while
// in-flight reservations exist understates the enforcement counter; with
// the settled raise to the same snapshot it strands budget==settled==DB
// with resv orphaned (the r44 shape). resv is read INSIDE the script so
// migrateOrCheckInvariant maintains the F1′ Redis-side triple
// (budget == budgetset + budgetresv). First sight of a scope initializes
// budgetset from the ledger snapshot and budgetresv from the (post-raise)
// overhang — until then the invariant is undefined and NOT alerted on
// (avoids false alarms during migration). Afterwards any drift beyond noise
// is a corruption signal (lost settle, double apply, alien writer) and is
// logged as such; drift also implies settled != SUM(DB), the audit-ring
// condition documented with the replay-repair semantics.
func (s *BudgetCalibrationService) migrateOrCheckInvariant(ctx context.Context, scope string, targetID int64, pk, period string, spent float64) error {
	bKey := fmt.Sprintf("budget:%s:%d:%s", scope, targetID, pk)
	rKey := budgetResvKey(scope, fmt.Sprintf("%d", targetID), pk)
	setKey := budgetSettledKey(scope, fmt.Sprintf("%d", targetID), pk)
	migKey := fmt.Sprintf("budgetmig:%s:%d:%s", scope, targetID, pk)
	ttl := PeriodTTL(period)

	ok, err := s.rdb.SetNX(ctx, migKey, "1", ttl).Result()
	if err != nil {
		slog.Warn("budget calibration: migration flag write failed", "key", bKey, "error", err)
		return err
	}
	if ok {
		// Drift fix (r35 negative-resv root cause, repro T1 2026-10-21):
		// first-sight must only seed counters for a namespace that PREDATES
		// the F1' split. When reserve/settle writers are already active
		// (budgetset/budgetresv keys exist), re-deriving resv from the
		// overhang clobbers LIVE reservations — their settles then drive
		// resv negative while the raise path pushes budget/settled to the
		// ledger sum (budget==settled==DB, resv<0). Skip the seed entirely;
		// the r-aware raise above keeps the counters consistent from here.
		if s.rdb.Exists(ctx, rKey).Val() != 0 || s.rdb.Exists(ctx, setKey).Val() != 0 {
			slog.Info("budget calibration: migration flag first-seen but F1' counters already active — leaving resv untouched; settled raise below owns the audit counter",
				"key", bKey)
		} else {
			cur, _ := s.rdb.Get(ctx, bKey).Float64()
			over := cur - spent
			if over < 0 {
				over = 0
			}
			s.rdb.Set(ctx, setKey, strconv.FormatFloat(spent, 'f', -1, 64), ttl)
			s.rdb.Set(ctx, rKey, strconv.FormatFloat(over, 'f', -1, 64), ttl)
			slog.Info("budget calibration: F1' counters migrated (settled=SUM(DB), resv=overhang)",
				"key", bKey, "settled", spent, "resv", over)
			return nil
		}
	}
	// RAISE-ONLY settled sync (r34): budgetset mirrors SUM(DB usage_logs) —
	// every settle's cost has exactly one ledger row, so settled can only
	// LEGITIMATELY LAG (a settle lost by every recovery tier). Raising it
	// here closes that lag within one round and keeps the invariant check
	// meaningful (a settled above SUM(DB) would be a double-apply signal —
	// never lowered by this path). Without this, lost settles left the r33c
	// drift (settled 19.696 vs DB 20.946) as a standing WARN.
	stCur, _ := s.rdb.Get(ctx, setKey).Float64()
	if stCur < spent {
		s.rdb.Set(ctx, setKey, strconv.FormatFloat(spent, 'f', -1, 64), ttl)
		slog.Warn("budget calibration: settled RAISED to ledger sum (lost settles healed into the audit counter)",
			"key", setKey, "settled_was", stCur, "settled_now", spent)
	}
	b, _ := s.rdb.Get(ctx, bKey).Float64()
	st, _ := s.rdb.Get(ctx, setKey).Float64()
	rv, _ := s.rdb.Get(ctx, rKey).Float64()
	// Shadow quarantines (r78v): expired holds the reaper moved out of resv
	// are still counted inside budget; the invariant gains the shadow term.
	sh, _ := s.rdb.Get(ctx, budgetShadowKey(scope, fmt.Sprintf("%d", targetID), pk)).Float64()
	drift := b - st - rv - sh
	if drift < 0 {
		drift = -drift
	}
	if drift > 0.01 {
		slog.Warn("budget calibration: INVARIANT DRIFT budget != settled+resv+shadow (corruption signal; settled-vs-DB audit applies; replay-repair per decision-packages §A.2-impl)",
			"key", bKey, "budget", b, "settled", st, "resv", rv, "shadow", sh, "drift", drift)
	}
	return nil
}

func periodBoundaries(period string) (start, end time.Time) {
	now := time.Now().UTC()
	switch period {
	case "daily":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case "weekly":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start = time.Date(now.Year(), now.Month(), now.Day()-weekday+1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 7)
	case "monthly":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	default:
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	}
	return
}
