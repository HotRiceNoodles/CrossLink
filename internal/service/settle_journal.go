package service

// D-R1 durable settle journal (2026-10-22; reviewed twice):
//
//   v1 review: the usage-scan reconcile was replaced by this journal (its
//   "was it billed?" discriminator — settledmark presence — expires with
//   the mark TTL, and its watermark coupling raced concurrent calibration).
//
//   v2 review (this revision) hardens three pointed counterexamples:
//   (1) replay/compaction snapshot-then-rewrite raced live appends — the
//       rewrite could drop intents appended after the snapshot. Fixed by
//       SEGMENT ROTATION: the active journal is sealed (renamed) under the
//       append mutex, a fresh active file takes over, and only sealed
//       segments are ever processed — live appends always land in the new
//       active file and cannot be lost by a rewrite.
//   (2) open/boot-replay failures were warn-and-serve — not a recovery
//       gate. Fixed by a STRICT boot gate: an unopenable journal is fatal
//       (billing-integrity component), and serving starts only after a
//       completed locked replay round (retry loop, loud).
//   (3) holding the calibration lock only excludes CONCURRENT raises; a
//       raise that already absorbed a row's cost (settledmark absent)
//       would be double-counted by a later replay. Fixed on the RAISE
//       side: every calibration round now CERTIFIES the rows it absorbs
//       by writing their settledmarks (mark meaning: "this row's cost is
//       in the counters by SOME path"), bounded by the round's watermark;
//       replay skips certified lines — the durable idempotency between
//       raise and unresolved intents. Replay additionally verifies lock
//       OWNERSHIP and renews the lease between batches, aborting the
//       segment (lines kept) on loss so an expired lease can never race
//       a new lock holder.
//
// Durability level (unchanged): O_APPEND write(2) survives PROCESS KILL
// (page cache is OS-owned). POWER LOSS is explicitly NOT claimed — the
// pre-existing layers (usage WAL→PG rows + hourly raise-only calibration)
// remain that floor.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

var (
	settleJournalRecoveredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_journal_recovered_total",
		Help: "Settles recovered by the journal replay (process-kill class).",
	})
	settleJournalExpiredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_journal_expired_total",
		Help: "Journal lines past the settle guard window: dropped, hourly calibration raise owns the heal (under-count direction).",
	})
	settleJournalKeptTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_journal_kept_total",
		Help: "Journal lines kept for retry (Redis unavailable, lease lost, lock unavailable).",
	})
	settleJournalMalformedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_journal_malformed_total",
		Help: "Journal lines dropped as malformed/unparseable (truncated tail after a crash, corruption): auditable loss, healed by DB-based calibration.",
	})
	settleJournalWriteFailTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_journal_write_fail_total",
		Help: "Journal append failures: the crash-recovery floor is degraded for those tasks (loud, never silent).",
	})
	settleJournalLeaseLostTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_budget_settle_journal_lease_lost_total",
		Help: "Replay segments aborted because the calibration-lock lease was lost or expired mid-round.",
	})
)

// ErrJournalLockNotAcquired: the calibration lock could not be taken — the
// caller retries (boot gate loops; maintenance defers).
var ErrJournalLockNotAcquired = errors.New("settle journal replay: calibration lock not acquired in time")

// ErrJournalLeaseLost: ownership verification failed mid-round; remaining
// lines stay in their sealed segment for the next round.
var ErrJournalLeaseLost = errors.New("settle journal replay: calibration lock lease lost mid-round")

type journalLine struct {
	Scope   string  `json:"scope"`
	Target  string  `json:"target"`
	Period  string  `json:"period"`
	Pk      string  `json:"pk"`
	ReqID   string  `json:"request_id"`
	Cost    float64 `json:"cost"`
	Ts      int64   `json:"ts"`
	Version int     `json:"v"`
}

func (l journalLine) task() settleRetryTask {
	return settleRetryTask{scope: l.Scope, targetID: l.Target, period: l.Period, pk: l.Pk, reqID: l.ReqID, cost: l.Cost}
}

const (
	journalActiveName  = "journal.jsonl"
	journalSealedGlob  = "journal.*.sealed"
	journalSealPrefix  = "journal."
	journalSealSuffix  = ".sealed"
	journalBatchSize   = 1000
	journalSegDeadline = 10 * time.Minute // soft per-round budget; remaining segments wait sealed
)

// settleJournal: appends go ONLY to the active file; processing works ONLY
// on sealed segments (sealed under mu), so a replay/compaction can never
// race an in-flight append.
type settleJournal struct {
	mu   sync.Mutex
	dir  string
	path string
	f    *os.File
}

func openSettleJournal(dir string) (*settleJournal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("settle journal mkdir: %w", err)
	}
	path := filepath.Join(dir, journalActiveName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("settle journal open: %w", err)
	}
	return &settleJournal{dir: dir, path: path, f: f}, nil
}

// OpenSettleJournal arms the crash floor. Callers treat an error as FATAL
// (billing-integrity component unavailable) — the strict boot gate.
func (s *BudgetService) OpenSettleJournal(dir string) error {
	j, err := openSettleJournal(dir)
	if err != nil {
		return err
	}
	s.journal = j
	return nil
}

// CloseSettleJournal releases the active-file handle. Production never calls
// it (the journal lives for the process lifetime); tests need it so
// t.TempDir cleanup can delete the directory on Windows, where an open
// handle locks the file (same constraint as the usage spool's Close).
func (s *BudgetService) CloseSettleJournal() {
	if s.journal == nil {
		return
	}
	s.journal.mu.Lock()
	defer s.journal.mu.Unlock()
	if s.journal.f != nil {
		s.journal.f.Close()
		s.journal.f = nil
	}
}

// append writes one intent to the ACTIVE file (never a sealed segment).
func (j *settleJournal) append(t settleRetryTask) error {
	b, err := json.Marshal(journalLine{
		Scope: t.scope, Target: t.targetID, Period: t.period, Pk: t.pk,
		ReqID: t.reqID, Cost: t.cost, Ts: time.Now().Unix(), Version: 1,
	})
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return errors.New("settle journal not open")
	}
	_, err = j.f.Write(append(b, '\n'))
	return err
}

// sealActive rotates: renames the active file to a sealed segment and
// opens a fresh active file — atomically under mu, so every append either
// landed in the old file (now sealed) or the new one. Returns the sealed
// path ("" when the active file was empty).
func (j *settleJournal) sealActive() (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	st, err := os.Stat(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if st.Size() == 0 {
		return "", nil
	}
	if j.f != nil {
		j.f.Close()
		j.f = nil
	}
	sealed := filepath.Join(j.dir, journalSealPrefix+fmt.Sprintf("%d", time.Now().UnixNano())+journalSealSuffix)
	if err := os.Rename(j.path, sealed); err != nil {
		return "", err
	}
	nf, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	j.f = nf
	return sealed, nil
}

// sealedSegments lists sealed segments oldest-first (names embed unixnano).
func (j *settleJournal) sealedSegments() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(j.dir, journalSealedGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// SettleScopeRef names one scope of a request's settle plan.
type SettleScopeRef struct {
	Scope, TargetID, Period string
}

// JournalSettleIntents durably records ALL of a request's scope-settle
// intents BEFORE any per-scope settle begins (r78i v1: scopes settle
// sequentially and a kill between them left the never-reached scopes with
// no trace — org recovered 0/8). Per-scope appends in SettleScope remain
// as defense; duplicates dedup by settledmark at replay.
func (s *BudgetService) JournalSettleIntents(reqID string, cost float64, scopes []SettleScopeRef, reservations *BudgetReservations) {
	if s.journal == nil || reqID == "" || len(scopes) == 0 {
		return
	}
	for _, sc := range scopes {
		pk := ""
		if reservations != nil {
			pk = reservations.PeriodKeys[sc.Scope]
		}
		if pk == "" {
			pk = PeriodKey(sc.Period)
		}
		if err := s.journal.append(settleRetryTask{
			scope: sc.Scope, targetID: sc.TargetID, period: sc.Period,
			pk: pk, reqID: reqID, cost: cost,
		}); err != nil {
			settleJournalWriteFailTotal.Inc()
			slog.Warn("settle journal intent append failed (crash floor degraded for this scope)",
				"request_id", reqID, "scope", sc.Scope, "error", err)
		}
	}
}

// SettleJournalStats reports one replay pass.
type SettleJournalStats struct {
	Total         int
	AlreadyMarked int
	Recovered     int
	Expired       int
	KeptForRetry  int
}

// journalBatchProbe is a test seam between batches (nil in production):
// inject lease corruption / concurrent writes at precise points.
var journalBatchProbe func()

// lockRenewScript atomically verifies ownership and extends the lease:
// 1 = still owner (renewed), 0 = lost.
var lockRenewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// replaySegment processes ONE sealed segment. Kept lines are re-appended
// to the ACTIVE file only AFTER the segment was fully processed; on a
// kept-append failure the segment is RETAINED (lines never dropped). If
// the segment is removed but kept-append failed midway, the yet-unappended
// kept lines would be lost — so appends happen first into the active file,
// and only then is the segment deleted; duplicates between the two dedup
// at the next replay.
func (s *BudgetService) replaySegment(ctx context.Context, seg string, verify func() error) (*SettleJournalStats, error) {
	st := &SettleJournalStats{}
	guard := s.markerTTL + settleRetryHorizon + time.Hour
	now := time.Now()

	f, err := os.Open(seg)
	if err != nil {
		return nil, fmt.Errorf("segment open: %w", err)
	}
	// Close BEFORE the trailing os.Remove(seg): Windows refuses to delete a
	// file with an open handle (defer would close only at return, after the
	// remove already failed). The scanner is the only reader; the settle
	// phase below never touches f.
	var lines []journalLine
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var l journalLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil || l.ReqID == "" {
			settleJournalMalformedTotal.Inc()
			slog.Warn("settle journal replay: malformed line dropped (auditable; DB calibration heals the row)",
				"segment", filepath.Base(seg))
			continue
		}
		st.Total++
		key := l.Scope + "\x1f" + l.Target + "\x1f" + l.ReqID
		if seen[key] {
			continue
		}
		seen[key] = true
		if now.Sub(time.Unix(l.Ts, 0)) > guard {
			st.Expired++
			settleJournalExpiredTotal.Inc()
			slog.Warn("settle journal EXPIRED (guard window passed; hourly calibration owns the heal)",
				"scope", l.Scope, "target", l.Target, "request_id", l.ReqID, "cost", l.Cost)
			continue
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		f.Close()
		// Segment retained: scan error must not drop lines.
		return nil, fmt.Errorf("segment scan: %w", err)
	}
	f.Close() // before Remove: Windows cannot delete an open file

	var kept []journalLine
	for i := 0; i < len(lines); i += journalBatchSize {
		if journalBatchProbe != nil {
			journalBatchProbe()
		}
		if verify != nil {
			if verr := verify(); verr != nil {
				// Lease lost: everything not yet processed stays (this
				// batch included) — never process unverified.
				kept = append(kept, lines[i:]...)
				settleJournalLeaseLostTotal.Inc()
				slog.Warn("settle journal segment aborted: calibration-lock lease lost",
					"segment", filepath.Base(seg), "kept", len(kept))
				break
			}
		}
		end := i + journalBatchSize
		if end > len(lines) {
			end = len(lines)
		}
		pipe := s.rdb.Pipeline()
		cmds := make([]*redis.IntCmd, 0, end-i)
		for _, l := range lines[i:end] {
			t := l.task()
			cmds = append(cmds, pipe.Exists(ctx, settledMarkKey(t.scope, t.targetID, t.reqID)))
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			kept = append(kept, lines[i:end]...)
			continue
		}
		for k, l := range lines[i:end] {
			t := l.task()
			mk, err := cmds[k].Result()
			if err != nil {
				kept = append(kept, l)
				continue
			}
			if mk == 1 {
				st.AlreadyMarked++
				continue
			}
			tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, serr := s.runSettleScript(tctx, t)
			cancel()
			if serr != nil {
				kept = append(kept, l)
				continue
			}
			st.Recovered++
			settleJournalRecoveredTotal.Inc()
			slog.Warn("settle journal RECOVERED (process-kill class)",
				"scope", t.scope, "target", t.targetID, "request_id", t.reqID, "cost", t.cost)
		}
	}

	st.KeptForRetry = len(kept)
	settleJournalKeptTotal.Add(float64(len(kept)))
	// Kept lines go to the ACTIVE file BEFORE the segment is removed; if an
	// append fails midway the segment is retained (the already-appended
	// copies dedup at the next replay).
	for _, l := range kept {
		if aerr := s.journal.append(l.task()); aerr != nil {
			return st, fmt.Errorf("kept-line re-append failed: %w", aerr)
		}
	}
	if err := os.Remove(seg); err != nil && !os.IsNotExist(err) {
		return st, fmt.Errorf("segment remove: %w", err)
	}
	return st, nil
}

// SettleJournalRound seals the active journal, then processes every sealed
// segment under the CALIBRATION LOCK with per-batch lease verification and
// renewal. Lock unavailable -> ErrJournalLockNotAcquired (nothing sealed is
// lost; segments wait for the next round).
func (s *BudgetService) SettleJournalRound(ctx context.Context, calSvc *BudgetCalibrationService, lockWait time.Duration) (*SettleJournalStats, error) {
	if calSvc == nil {
		return nil, errors.New("settle journal round: nil calibration service (lock owner)")
	}
	// Seal FIRST (cheap, no lock needed): appends continue into the fresh
	// active file while sealed segments are processed.
	if _, err := s.journal.sealActive(); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	segs, err := s.journal.sealedSegments()
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return &SettleJournalStats{}, nil
	}
	deadline := time.Now().Add(lockWait)
	for {
		token, release, err := calSvc.acquireCalibrationLock(ctx)
		if err == nil && release != nil {
			total := &SettleJournalStats{}
			segDeadline := time.Now().Add(journalSegDeadline)
			verify := func() error {
				ok, verr := lockRenewScript.Run(ctx, s.rdb,
					[]string{calibrationLockKey}, token,
					int64((calibrationLockTTL - 5*time.Minute).Milliseconds())).Int()
				if verr != nil {
					return verr
				}
				if ok != 1 {
					return ErrJournalLeaseLost
				}
				return nil
			}
			var roundErr error
			for _, seg := range segs {
				if time.Now().After(segDeadline) {
					break // remaining segments stay sealed for the next round
				}
				st, serr := s.replaySegment(ctx, seg, verify)
				if st != nil {
					total.Total += st.Total
					total.AlreadyMarked += st.AlreadyMarked
					total.Recovered += st.Recovered
					total.Expired += st.Expired
					total.KeptForRetry += st.KeptForRetry
				}
				if serr != nil {
					roundErr = serr
					break
				}
			}
			release()
			if roundErr == nil && total.KeptForRetry > 0 {
				// Unresolved lines remain: the round did NOT complete — the
				// boot gate keeps waiting, maintenance retries.
				return total, fmt.Errorf("settle journal round incomplete: %d lines kept for retry", total.KeptForRetry)
			}
			return total, roundErr
		}
		if err != nil && !errors.Is(err, ErrCalibrationLockHeld) {
			return nil, fmt.Errorf("settle journal round: lock store unavailable: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, ErrJournalLockNotAcquired
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// SettleJournalBootGate is the STRICT recovery gate: it retries until one
// full round completes (lock acquired, store healthy). Serving billable
// traffic must not start before this returns nil.
func (s *BudgetService) SettleJournalBootGate(ctx context.Context, calSvc *BudgetCalibrationService) error {
	backoff := time.Second
	for {
		_, err := s.SettleJournalRound(ctx, calSvc, 10*time.Second)
		if err == nil {
			return nil
		}
		slog.Warn("settle journal BOOT REPLAY incomplete — retrying (serving waits)",
			"error", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// JournalLiveIntents counts journal intents (active file + sealed
// segments) whose age is inside the guard window — i.e. still replayable.
func (s *BudgetService) JournalLiveIntents(guard time.Duration) int64 {
	if s.journal == nil {
		return 0
	}
	var n int64
	now := time.Now()
	count := func(path string) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var l journalLine
			if json.Unmarshal(sc.Bytes(), &l) == nil && l.ReqID != "" {
				if now.Sub(time.Unix(l.Ts, 0)) <= guard {
					n++
				}
			}
		}
	}
	count(s.journal.path)
	if segs, err := s.journal.sealedSegments(); err == nil {
		for _, seg := range segs {
			count(seg)
		}
	}
	return n
}

// SettleJournalMaintenance periodically re-runs rounds (drives kept lines,
// compacts settled ones). A round that cannot take the lock defers.
func (s *BudgetService) SettleJournalMaintenance(ctx context.Context, calSvc *BudgetCalibrationService) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Reap first (idempotent): marks that expired since the last
			// round release their estimates before any reconciliation.
			if n := s.ReapExpiredReservations(ctx, time.Now()); n > 0 {
				slog.Warn("maintenance reaper released expired reservations", "count", n)
			}
			if _, err := s.SettleJournalRound(ctx, calSvc, 0); err != nil {
				slog.Warn("settle journal maintenance round deferred", "error", err)
			}
		}
	}
}
