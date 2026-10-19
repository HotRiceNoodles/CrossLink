package service

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Deterministic interleaving tests for the calibration write vs live
// Reserve/Adjust traffic (review requirement): the calibration lock only
// mutual-excludes calibrators, so the write must be safe against concurrent
// reservations and post-snapshot consumption on key/team/org scopes, with
// the divergence direction recorded in every assertion.

func runCalibratedWrite(t *testing.T, rdb *redis.Client, token, key string, spent float64, ttlSec int) int {
	t.Helper()
	res, err := calibratedWriteScript.Run(context.Background(), rdb,
		[]string{calibrationLockKey, key}, token, spent, ttlSec).Int()
	if err != nil {
		t.Fatalf("calibratedWriteScript: %v", err)
	}
	return res
}

func mustGetFloat(t *testing.T, rdb *redis.Client, key string) float64 {
	t.Helper()
	v, err := rdb.Get(context.Background(), key).Float64()
	if err != nil {
		t.Fatalf("GET %s: %v", key, err)
	}
	return v
}

// Steady-state interleaving (the erasure hazard): counters already equal the
// settled spend; an in-flight reservation pushes each counter ABOVE the
// (stale) snapshot; the calibration write must not lower any of them; the
// settlement's (cost-reserved) adjust then lands exactly.
func TestCalibrationInterleave_InflightReserveSurvivesWrite_AllScopes(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	s := NewBudgetService(rdb)
	ctx := context.Background()

	scopes := []BudgetScope{
		{Scope: "key", ID: "990103", Period: "monthly", Limit: 1000},
		{Scope: "team", ID: "990102", Period: "monthly", Limit: 1000},
		{Scope: "org", ID: "990101", Period: "monthly", Limit: 1000},
	}
	const settled = 100.0
	const estimate = 5.0
	const actual = 3.0

	// settled spend present in both DB (snapshot source) and counters
	for _, sc := range scopes {
		s.ReportUsage(ctx, sc.Scope, sc.ID, sc.Period, settled)
	}

	// request reserves while the (stale) snapshot == settled is being SUMmed
	res, exceeded := s.ReserveForRequest(ctx, scopes, estimate, "")
	if exceeded != "" {
		t.Fatalf("reserve unexpectedly exceeded at %s", exceeded)
	}
	for _, sc := range scopes {
		if _, ok := res.Reserved[sc.Scope]; !ok {
			t.Fatalf("scope %s: reservation not recorded on healthy path", sc.Scope)
		}
	}

	// calibration writes the stale snapshot (taken before the reserve)
	rdb.Set(context.Background(), calibrationLockKey, "tok", time.Minute)
	for _, sc := range scopes {
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		runCalibratedWrite(t, rdb, "tok", key, settled, 3600)
		before := mustGetFloat(t, rdb, key)
		if before != settled+estimate {
			t.Fatalf("scope %s: counter %.6f — in-flight reserve erased by calibration write (direction: under-count)",
				sc.Scope, before)
		}
	}

	// settlement: adjust (actual - reserved) on each scope
	for _, sc := range scopes {
		s.AdjustBudgetKeyed(ctx, sc.Scope, sc.ID, sc.Period, res.PeriodKeys[sc.Scope], actual-res.Reserved[sc.Scope])
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		if got := mustGetFloat(t, rdb, key); got != settled+actual {
			t.Fatalf("scope %s: final %.6f != exact %.6f (direction: %s)", sc.Scope, got, settled+actual,
				map[bool]string{true: "over-count", false: "under-count"}[got > settled+actual])
		}
	}
}

// Deficit-regime boundary: counter BELOW the snapshot while a reservation is
// in flight — the raise absorbs the reserve and the settlement lands short by
// the estimate; the NEXT round's write closes it. Asserts both the bounded
// short-fall and the exact recovery, direction recorded.
func TestCalibrationInterleave_DeficitRegime_BoundedThenClosedByNextRound(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	s := NewBudgetService(rdb)
	ctx := context.Background()

	scopes := []BudgetScope{
		{Scope: "key", ID: "990103", Period: "monthly", Limit: 1000},
		{Scope: "team", ID: "990102", Period: "monthly", Limit: 1000},
		{Scope: "org", ID: "990101", Period: "monthly", Limit: 1000},
	}
	const snapshotDB = 100.0 // ledger truth incl. outage-era rows
	const estimate = 5.0
	const actual = 3.0

	// outage left counters at 0 (fail-open under-count), ledger at snapshotDB.
	// In-flight request reserves on top of the deficit...
	res, exceeded := s.ReserveForRequest(ctx, scopes, estimate, "")
	if exceeded != "" {
		t.Fatalf("reserve exceeded at %s", exceeded)
	}

	// ...and the calibration writes the snapshot mid-flight.
	rdb.Set(context.Background(), calibrationLockKey, "tok", time.Minute)
	for _, sc := range scopes {
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		runCalibratedWrite(t, rdb, "tok", key, snapshotDB, 3600)
	}

	// settlement lands (actual - reserved) net-negative
	for _, sc := range scopes {
		s.AdjustBudgetKeyed(ctx, sc.Scope, sc.ID, sc.Period, res.PeriodKeys[sc.Scope], actual-res.Reserved[sc.Scope])
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		got := mustGetFloat(t, rdb, key)
		wantShort := snapshotDB + actual - estimate // documented boundary
		if got != wantShort {
			t.Fatalf("scope %s: %.6f != documented short-fall %.6f", sc.Scope, got, wantShort)
		}
		if got >= snapshotDB+actual {
			t.Fatalf("scope %s: expected under-count boundary, got over-count", sc.Scope)
		}
	}

	// next hourly round: request long settled, fresh SUM == snapshotDB+actual.
	for _, sc := range scopes {
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		runCalibratedWrite(t, rdb, "tok", key, snapshotDB+actual, 3600)
		if got := mustGetFloat(t, rdb, key); got != snapshotDB+actual {
			t.Fatalf("scope %s: next round did not close the gap: %.6f != %.6f", sc.Scope, got, snapshotDB+actual)
		}
	}
}

// The write in isolation: raise-only in both directions, creation, garbage
// rebuild, and ownership loss.
func TestCalibratedWrite_Directions(t *testing.T) {
	rdb, _ := newBudgetTestRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)

	// counter above snapshot (in-flight reserve + newer consumption): NO-OP
	rdb.Set(ctx, "budget:key:1:2026-01", "105", time.Hour)
	runCalibratedWrite(t, rdb, "tok", "budget:key:1:2026-01", 100, 3600)
	if got := mustGetFloat(t, rdb, "budget:key:1:2026-01"); got != 105 {
		t.Fatalf("write LOWERED counter to %v — erases in-flight reservations", got)
	}

	// counter below snapshot (under-count): raised to snapshot
	rdb.Set(ctx, "budget:key:2:2026-01", "90", time.Hour)
	runCalibratedWrite(t, rdb, "tok", "budget:key:2:2026-01", 100, 3600)
	if got := mustGetFloat(t, rdb, "budget:key:2:2026-01"); got != 100 {
		t.Fatalf("raise failed: %v", got)
	}

	// missing counter: created at snapshot with TTL
	if res := runCalibratedWrite(t, rdb, "tok", "budget:key:3:2026-01", 100, 3600); res != 1 {
		t.Fatalf("expected creation result 1, got %d", res)
	}
	if got := mustGetFloat(t, rdb, "budget:key:3:2026-01"); got != 100 {
		t.Fatalf("created value %v != 100", got)
	}
	if ttl, _ := rdb.TTL(ctx, "budget:key:3:2026-01").Result(); ttl <= 0 {
		t.Fatalf("created key missing TTL: %v", ttl)
	}

	// garbage value: rebuilt from ledger snapshot (derived data)
	rdb.Set(ctx, "budget:key:4:2026-01", "not-a-number", time.Hour)
	runCalibratedWrite(t, rdb, "tok", "budget:key:4:2026-01", 100, 3600)
	if got := mustGetFloat(t, rdb, "budget:key:4:2026-01"); got != 100 {
		t.Fatalf("garbage not rebuilt: %v", got)
	}

	// ownership lost: -1 and counter untouched
	rdb.Set(ctx, "budget:key:5:2026-01", "42", time.Hour)
	if res := runCalibratedWrite(t, rdb, "wrong-token", "budget:key:5:2026-01", 100, 3600); res != -1 {
		t.Fatalf("expected ownership-lost -1, got %d", res)
	}
	if got := mustGetFloat(t, rdb, "budget:key:5:2026-01"); got != 42 {
		t.Fatalf("stale holder wrote: %v", got)
	}
}

// Phantom-reservation fix v2 — DEFINITELY-NOT-SENT class (here: server gone,
// dial error): NO reservation is recorded; the subsequent full-cost
// settlement is exact and never refunds a phantom increment.
func TestReserveForRequest_DefinitelyNotSent_RecordsNothing_FullCostSettleExact(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	mr.Close() // dial fails -> definitely-not-sent

	s := NewBudgetService(rdb)
	ctx := context.Background()
	scopes := []BudgetScope{
		{Scope: "key", ID: "990103", Period: "monthly", Limit: 1000},
		{Scope: "team", ID: "990102", Period: "monthly", Limit: 1000},
		{Scope: "org", ID: "990101", Period: "monthly", Limit: 1000},
	}
	res, exceeded := s.ReserveForRequest(ctx, scopes, 5.0, "")
	if exceeded != "" {
		t.Fatalf("fail-open must not exceed, got %s", exceeded)
	}
	if len(res.Reserved) != 0 {
		t.Fatalf("definitely-not-sent: phantom reservation recorded: %+v — settlement would refund increments that never landed", res.Reserved)
	}

	// after Redis recovers, settlement reports full cost: exact
	mr2 := miniredis.RunT(t)
	rdb2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { rdb2.Close() })
	s2 := NewBudgetService(rdb2)
	for _, sc := range scopes {
		s2.ReportUsage(ctx, sc.Scope, sc.ID, sc.Period, 3.0) // full-cost settlement
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		if got := mustGetFloat(t, rdb2, key); got != 3.0 {
			t.Fatalf("scope %s: settled %.6f != exact 3 (direction: %s)", sc.Scope, got,
				map[bool]string{true: "over-count", false: "under-count"}[got > 3.0])
		}
	}
}

// Phantom-reservation fix v2 — INDETERMINATE class (applied but response
// lost, e.g. a paused Redis executing queued commands on unpause): the
// reservation IS recorded so settlement lands (cost-reserved):
//   - landed (the paused-redis case): EXACT — reserve applied late, settle
//     applies the remainder, no double count;
//   - not landed: UNDER-count by the estimate — the divergence direction
//     the hourly raise-only calibration heals.
// Not recording this class double-counts the landed case (reserve + full
// cost) into an OVER-count that raise-only calibration can never remove.
func TestReserveForRequest_Indeterminate_RecordsReservation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	mr.Close() // dial error — but forced to classify as indeterminate below

	orig := redisWriteDefinitelyNotSent
	redisWriteDefinitelyNotSent = func(error) bool { return false } // force indeterminate
	t.Cleanup(func() { redisWriteDefinitelyNotSent = orig })

	s := NewBudgetService(rdb)
	ctx := context.Background()
	scopes := []BudgetScope{
		{Scope: "key", ID: "990103", Period: "monthly", Limit: 1000},
		{Scope: "team", ID: "990102", Period: "monthly", Limit: 1000},
		{Scope: "org", ID: "990101", Period: "monthly", Limit: 1000},
	}
	res, exceeded := s.ReserveForRequest(ctx, scopes, 5.0, "")
	if exceeded != "" {
		t.Fatalf("fail-open must not exceed, got %s", exceeded)
	}
	for _, sc := range scopes {
		if _, ok := res.Reserved[sc.Scope]; !ok {
			t.Fatalf("indeterminate class must record the reservation (settlement must land cost-reserved), scope %s missing", sc.Scope)
		}
	}
}

// End-to-end arithmetic of the indeterminate-landed case: the reserve's
// response is lost (recorded anyway), the queued command executes when the
// paused server resumes, then settlement lands (cost-reserved): final ==
// cost exactly — the paused-redis scenario (r22 observed direction).
func TestReserveForRequest_IndeterminateLanded_SettlesExact(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	mr.Close() // dial fails; classifier forced indeterminate below

	orig := redisWriteDefinitelyNotSent
	redisWriteDefinitelyNotSent = func(error) bool { return false }
	t.Cleanup(func() { redisWriteDefinitelyNotSent = orig })

	s := NewBudgetService(rdb)
	ctx := context.Background()
	scopes := []BudgetScope{
		{Scope: "key", ID: "990103", Period: "monthly", Limit: 1000},
		{Scope: "team", ID: "990102", Period: "monthly", Limit: 1000},
		{Scope: "org", ID: "990101", Period: "monthly", Limit: 1000},
	}
	const estimate = 5.0
	const actual = 3.0

	// reserve fails with a lost response — reservation recorded
	res, exceeded := s.ReserveForRequest(ctx, scopes, estimate, "")
	if exceeded != "" {
		t.Fatalf("fail-open must not exceed, got %s", exceeded)
	}

	// the server recovers; the queued reserve command now executes (+estimate
	// lands — simulated on a healthy instance), then settlement (cost-reserved)
	rdb2, _ := newBudgetTestRedis(t)
	s2 := NewBudgetService(rdb2)
	for _, sc := range scopes {
		if _, ok := res.Reserved[sc.Scope]; !ok {
			t.Fatalf("indeterminate class must record the reservation, scope %s missing", sc.Scope)
		}
		s2.applyBudgetDeltaKeyed(ctx, sc.Scope, sc.ID, sc.Period, res.PeriodKeys[sc.Scope], estimate)
		s2.AdjustBudgetKeyed(ctx, sc.Scope, sc.ID, sc.Period, res.PeriodKeys[sc.Scope], actual-res.Reserved[sc.Scope])
		key := "budget:" + sc.Scope + ":" + sc.ID + ":" + PeriodKey(sc.Period)
		if got := mustGetFloat(t, rdb2, key); got != actual {
			t.Fatalf("scope %s: settled %.6f != exact %.6f (landed indeterminate must not double-count)", sc.Scope, got, actual)
		}
	}
}

// The classifier: dial/pool-acquire failures are definitely-not-sent;
// timeouts and mid-flight resets are indeterminate.
func TestRedisWriteDefinitelyNotSent_Classification(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: refused")}
	if !redisWriteDefinitelyNotSent(dialErr) {
		t.Fatal("dial failure must classify as definitely-not-sent")
	}
	if !redisWriteDefinitelyNotSent(redis.ErrPoolTimeout) {
		t.Fatal("pool-acquire timeout must classify as definitely-not-sent")
	}
	ioTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if redisWriteDefinitelyNotSent(ioTimeout) {
		t.Fatal("i/o timeout after write must classify as INDETERMINATE (response may be lost after apply)")
	}
	resetErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	if redisWriteDefinitelyNotSent(resetErr) {
		t.Fatal("mid-flight reset must classify as INDETERMINATE")
	}
	if redisWriteDefinitelyNotSent(context.DeadlineExceeded) {
		t.Fatal("context deadline must classify as INDETERMINATE")
	}
}
