package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// X9 (review 2026-10-23 round 12): the ABS round's paired SUM+MAX ceiling
// is an ID bound, NOT a snapshot. PostgreSQL sequences allocate the id at
// INSERT time, BEFORE commit — a transaction that inserted a LOW id and is
// still open commits AFTER the round's SUM: the sum never sees its cost,
// but the later page queries (each their own READ COMMITTED snapshot) do
// see the row below the ceiling — proof lands ahead of cost and the cost
// never arrives (the cert blanks every later count). Deterministic with a
// REAL isolated PostgreSQL and two connections (user directive: do not
// close this window as "transactions are usually short"):
//
//	T1: INSERT low-id row (cost 7), keep the transaction OPEN
//	T2: INSERT high-id row (cost 5), COMMIT
//	round: SUM+MAX under one exported snapshot -> 5 / T2.id
//	(calibAbsSumProbe) COMMIT T1  — after the SUM, before the pages
//	pages: must NOT see T1's row (same exported snapshot)
//	next regular round: T1's row counted + certified exactly once.
//
// Gated on CL_TEST_PG_DSN (real isolation semantics; sqlite serializes
// writers and cannot reproduce). Run:
//	docker network + postgres, then
//	CL_TEST_PG_DSN="host=... user=postgres password=lab dbname=x9 sslmode=disable" \
//	  go test ./internal/service -run X9 -count=1 -v
func TestCalibrateAbs_X9_LateCommitLowIdNotProvedWithoutCost(t *testing.T) {
	dsn := os.Getenv("CL_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("X9 requires a real PostgreSQL (CL_TEST_PG_DSN); sqlite serializes writers and cannot reproduce the commit-after-SUM window")
	}
	ctx := context.Background()
	// Fresh schema per run (same DB, dropped table): full isolation.
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	if err := db.Exec("DROP TABLE IF EXISTS usage_logs").Error; err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cal := NewBudgetCalibrationService(db, rdb)
	cal.SetJournalGuard(time.Hour)
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })

	tid := int64(92)
	pk := PeriodKey("monthly")
	tids := "92"
	start, end := periodBoundaries("monthly")
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	// B3 class trigger: budgetset present, watermark absent.
	rdb.Set(ctx, budgetSettledKey("team", tids, pk), "0", time.Hour)

	// T1: low-id row, transaction held OPEN (sequence id allocated now).
	t1, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := t1.ExecContext(ctx,
		`INSERT INTO usage_logs (request_id, route_type, model_requested, model_used, team_id, cost, currency, status_code, created_at)
		 VALUES ('req-x9-low', 'openai', 'x9m', 'x9m', $1, 7.0, 'CNY', 200, now())`, tid); err != nil {
		t.Fatal(err)
	}
	// T2: high-id row, committed BEFORE the round.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO usage_logs (request_id, route_type, model_requested, model_used, team_id, cost, currency, status_code, created_at)
		 VALUES ('req-x9-high', 'openai', 'x9m', 'x9m', $1, 5.0, 'CNY', 200, now())`, tid); err != nil {
		t.Fatal(err)
	}

	// Deterministic interleaving: T1 commits AFTER the round's SUM, BEFORE
	// the pages (the probe is exactly that seam).
	calibAbsSumProbe = func() {
		if err := t1.Commit(); err != nil {
			t.Fatalf("probe commit T1: %v", err)
		}
	}
	t.Cleanup(func() { calibAbsSumProbe = nil })

	if _, err := cal.calibrateTarget(ctx, "tok", "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("abs round: %v", err)
	}

	// THE assertion: T1's row must NOT carry proof (its cost was not in
	// the ABS target). Pre-fix this fails — the page's own READ COMMITTED
	// snapshot saw the just-committed low-id row under the ceiling and
	// certified it, permanently blanking its cost.
	if rdb.Exists(ctx, fmt.Sprintf("calibcert:team:%s:req-x9-low", tids)).Val() != 0 {
		t.Fatal("X9: late-committing LOW-id row was PROVED without its cost (cert set while the ABS sum excluded it) — the ceiling is not a snapshot")
	}
	if got := getF(t, rdb, budgetSettledKey("team", tids, pk)); got != 5.0 {
		t.Fatalf("abs settled = %v, want 5 (T2 only; T1 invisible to the round)", got)
	}

	// Next REGULAR round: T1's row is now committed — it must be counted
	// and certified exactly once, closing on the full ledger truth.
	calibAbsSumProbe = nil
	if _, err := cal.calibrateTarget(ctx, "tok", "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("regular round: %v", err)
	}
	if rdb.Exists(ctx, fmt.Sprintf("calibcert:team:%s:req-x9-low", tids)).Val() != 1 {
		t.Fatal("regular round must certify T1's row once counted")
	}
	if got := getF(t, rdb, budgetSettledKey("team", tids, pk)); got != 12.0 {
		t.Fatalf("final settled = %v, want 12 (7+5, exactly once each)", got)
	}
	if got := getF(t, rdb, fmt.Sprintf("budget:team:%s:%s", tids, pk)); got != 12.0 {
		t.Fatalf("final budget = %v, want 12", got)
	}
}

// X10a (r78x): on PostgreSQL a REAL snapshot-export failure (pg_export_snapshot
// revoked from the running role) must FAIL THE ROUND — no proofs, no
// watermark — and never silently fall back to the plain paired query whose
// page path the X9 counterexample falsified.
func TestCalibrateAbs_X10a_ExportFailureFailsRoundClosed(t *testing.T) {
	dsn := os.Getenv("CL_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("X10a requires a real PostgreSQL (CL_TEST_PG_DSN)")
	}
	ctx := context.Background()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	if err := db.Exec("DROP TABLE IF EXISTS usage_logs").Error; err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlDB, _ := db.DB()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	tid := int64(93)
	pk := PeriodKey("monthly")
	start, end := periodBoundaries("monthly")
	tids := "93"
	wmKey := fmt.Sprintf("budgetaudit:team:%s:%s", tids, pk)
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	rdb.Set(ctx, budgetSettledKey("team", tids, pk), "0", time.Hour)
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO usage_logs (request_id, route_type, model_requested, model_used, team_id, cost, currency, status_code, created_at)
		 VALUES ('req-x10-a', 'openai', 'x9m', 'x9m', $1, 5.0, 'CNY', 200, now())`, tid); err != nil {
		t.Fatal(err)
	}
	// REAL failure injection: REVOKE the snapshot export from PUBLIC — the
	// superuser DSN would bypass the ACL, so the calibration service runs
	// under a NON-superuser role that inherits the denial.
	if _, err := sqlDB.ExecContext(ctx, `REVOKE EXECUTE ON FUNCTION pg_export_snapshot() FROM PUBLIC`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	t.Cleanup(func() {
		sqlDB.ExecContext(context.Background(), `GRANT EXECUTE ON FUNCTION pg_export_snapshot() TO PUBLIC`)
	})
	// Idempotent role setup (a prior run's grants make plain DROP ROLE fail).
	sqlDB.ExecContext(ctx, `DROP OWNED BY x10`)
	sqlDB.ExecContext(ctx, `DROP ROLE IF EXISTS x10`)
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE ROLE x10 LOGIN PASSWORD 'lab'`); err != nil {
		t.Fatalf("role: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.Background(), context.Background()
		sqlDB.ExecContext(c, `GRANT EXECUTE ON FUNCTION pg_export_snapshot() TO PUBLIC`)
		sqlDB.ExecContext(cc, `DROP OWNED BY x10`)
		sqlDB.ExecContext(cc, `DROP ROLE IF EXISTS x10`)
	})
	sqlDB.ExecContext(ctx, `GRANT USAGE ON SCHEMA public TO x10`)
	sqlDB.ExecContext(ctx, `GRANT SELECT ON usage_logs TO x10`)
	dsnX10 := strings.Replace(dsn, "user=postgres", "user=x10 password=lab", 1)
	dbX10, err := gorm.Open(postgres.Open(dsnX10), &gorm.Config{})
	if err != nil {
		t.Fatalf("open pg as x10: %v", err)
	}
	cal := NewBudgetCalibrationService(dbX10, rdb)
	cal.SetJournalGuard(time.Hour)
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })

	_, rerr := cal.calibrateTarget(ctx, "tok", "team", tid, pk, "monthly", start, end)
	if rerr == nil {
		t.Fatal("export failure must FAIL the ABS round (got nil error — silent fallback would reopen the X9 window)")
	}
	if !strings.Contains(rerr.Error(), "abs round") {
		t.Fatalf("error should name the ABS snapshot path: %v", rerr)
	}
	// Fail-closed artifacts: no proofs, no watermark, lock still ours.
	if rdb.Exists(ctx, fmt.Sprintf("calibcert:team:%s:req-x10-a", tids)).Val() != 0 {
		t.Fatal("proof must not land on a failed round")
	}
	if rdb.Exists(ctx, settledMarkKey("team", tids, "req-x10-a")).Val() != 0 {
		t.Fatal("settledmark must not land on a failed round")
	}
	if rdb.Exists(ctx, wmKey).Val() != 0 {
		t.Fatal("watermark must NOT advance on a failed round")
	}
	// Restore and verify the SAME target converges on the next round.
	if _, err := sqlDB.ExecContext(ctx, `GRANT EXECUTE ON FUNCTION pg_export_snapshot() TO PUBLIC`); err != nil {
		t.Fatal(err)
	}
	if _, err := cal.calibrateTarget(ctx, "tok", "team", tid, pk, "monthly", start, end); err != nil {
		t.Fatalf("round after restore: %v", err)
	}
	if got := getF(t, rdb, budgetSettledKey("team", tids, pk)); got != 5.0 {
		t.Fatalf("settled after retry = %v, want 5", got)
	}
	if rdb.Exists(ctx, wmKey).Val() != 1 {
		t.Fatal("watermark must advance only on the successful round")
	}
}

// X10b (r78x): a BeginTx-class failure (seam at the exact decision point)
// fails the round closed the same way.
func TestCalibrateAbs_X10b_BeginTxFailureFailsRoundClosed(t *testing.T) {
	dsn := os.Getenv("CL_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("X10b requires a real PostgreSQL (CL_TEST_PG_DSN)")
	}
	ctx := context.Background()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	if err := db.Exec("DROP TABLE IF EXISTS usage_logs").Error; err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlDB, _ := db.DB()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cal := NewBudgetCalibrationService(db, rdb)
	cal.SetJournalGuard(time.Hour)
	cal.SetLiveIntentCheck(func(time.Duration) int64 { return 0 })

	tid := int64(94)
	pk := PeriodKey("monthly")
	start, end := periodBoundaries("monthly")
	tids := "94"
	wmKey := fmt.Sprintf("budgetaudit:team:%s:%s", tids, pk)
	rdb.Set(ctx, calibrationLockKey, "tok", time.Minute)
	rdb.Set(ctx, budgetSettledKey("team", tids, pk), "0", time.Hour)
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO usage_logs (request_id, route_type, model_requested, model_used, team_id, cost, currency, status_code, created_at)
		 VALUES ('req-x10-b', 'openai', 'x9m', 'x9m', $1, 5.0, 'CNY', 200, now())`, tid); err != nil {
		t.Fatal(err)
	}
	calibAbsTxBeginHook = func() error { return fmt.Errorf("injected begin failure") }
	t.Cleanup(func() { calibAbsTxBeginHook = nil })

	_, rerr := cal.calibrateTarget(ctx, "tok", "team", tid, pk, "monthly", start, end)
	if rerr == nil || !strings.Contains(rerr.Error(), "injected begin failure") {
		t.Fatalf("begin failure must fail the round: %v", rerr)
	}
	if rdb.Exists(ctx, fmt.Sprintf("calibcert:team:%s:req-x10-b", tids)).Val() != 0 {
		t.Fatal("proof must not land on a failed round")
	}
	if rdb.Exists(ctx, wmKey).Val() != 0 {
		t.Fatal("watermark must NOT advance on a failed round")
	}
}
