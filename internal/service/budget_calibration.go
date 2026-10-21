package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
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
	ticker := time.NewTicker(time.Hour)
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
	}
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
		for _, team := range periodTeams {
			spent := spendMap[team.ID]
			if lerr := s.syncRedisAndSnapshot(ctx, token, "team", team.ID, pk, period, spent, team.BudgetLimit); lerr != nil {
				return lerr
			}
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
		spendMap := make(map[int64]float64, len(periodRows))
		for _, r := range periodRows {
			spendMap[r.OrgID] = r.Spent
		}
		for _, org := range periodOrgs {
			spent := spendMap[org.ID]
			if lerr := s.syncRedisAndSnapshot(ctx, token, "org", org.ID, pk, period, spent, org.BudgetLimit); lerr != nil {
				return lerr
			}
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
		spendMap := make(map[int64]float64, len(periodRows))
		for _, r := range periodRows {
			spendMap[r.APIKeyID] = r.Spent
		}
		for _, k := range periodKeys {
			spent := spendMap[k.ID]
			if lerr := s.syncRedisAndSnapshot(ctx, token, "key", k.ID, pk, period, spent, k.MaxBudget); lerr != nil {
				return lerr
			}
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
// the raise is atomic with respect to concurrent reserve/settle writers.
var calibratedWriteScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return -1
end
local cur = redis.call('GET', KEYS[2])
local spent = tonumber(ARGV[2])
local rv = tonumber(redis.call('GET', KEYS[3])) or 0
local target = spent + rv
if not cur or not tonumber(cur) then
	redis.call('SET', KEYS[2], tostring(target), 'EX', ARGV[3])
	return 1
end
local curVal = tonumber(cur)
if curVal < target then
	redis.call('INCRBYFLOAT', KEYS[2], target - curVal)
end
redis.call('EXPIRE', KEYS[2], ARGV[3])
return 0`)

func (s *BudgetCalibrationService) syncRedisAndSnapshot(ctx context.Context, token, scope string, targetID int64, pk, period string, spent, budget float64) error {
	key := fmt.Sprintf("budget:%s:%d:%s", scope, targetID, pk)
	resvKey := budgetResvKey(scope, fmt.Sprintf("%d", targetID), pk)
	res, err := calibratedWriteScript.Run(ctx, s.rdb,
		[]string{calibrationLockKey, key, resvKey}, token, spent, int(PeriodTTL(period).Seconds())).Int()
	if err != nil {
		slog.Warn("budget calibration: calibrated write failed", "key", key, "error", err)
		return nil // store error (not ownership loss): skip this row, keep going
	}
	if res == -1 {
		return fmt.Errorf("%w: key=%s", ErrCalibrationLockLost, key)
	}

	// F1′ counters: one-time migration, then the invariant check and the
	// audit watermark every round.
	if err := s.migrateOrCheckInvariant(ctx, scope, targetID, pk, period, spent); err == nil {
		s.rdb.Set(ctx, fmt.Sprintf("budgetaudit:%s:%d:%s", scope, targetID, pk),
			fmt.Sprintf("%.9f|%d", spent, time.Now().Unix()), PeriodTTL(period))
	}

	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "target_type"}, {Name: "target_id"}, {Name: "period_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"spent", "budget"}),
	}).Create(&model.BudgetSnapshot{
		TargetType: scope,
		TargetID:   targetID,
		PeriodKey:  pk,
		Spent:      spent,
		Budget:     budget,
	}).Error; err != nil {
		slog.Warn("budget calibration: failed to save snapshot", "scope", scope, "id", targetID, "error", err)
	}
	return nil
}

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
	drift := b - st - rv
	if drift < 0 {
		drift = -drift
	}
	if drift > 0.01 {
		slog.Warn("budget calibration: INVARIANT DRIFT budget != settled+resv (corruption signal; settled-vs-DB audit applies; replay-repair per decision-packages §A.2-impl)",
			"key", bKey, "budget", b, "settled", st, "resv", rv, "drift", drift)
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
