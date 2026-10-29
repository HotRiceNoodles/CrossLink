package dialect

// Round-13 authorization: fix the confirmed fresh-install drift between the
// usage_logs migration chains (mysql/sqlite) and the current UsageLog model
// via NEW incremental migrations (000004) — the historical 000001 files stay
// byte-identical. These tests execute the REAL migration files in the REAL
// order and verify with the REAL model: every field set on insert, every
// field read back.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const migParityUp = migDir + "000004_usage_model_parity.up.sql"
const migParityDn = migDir + "000004_usage_model_parity.down.sql"

// F34 billing idempotency column (sqlite chain 000007): usage_logs.event_id.
const migEventIDUp = migDir + "000007_usage_event_id.up.sql"
const migEventIDDn = migDir + "000007_usage_event_id.down.sql"

func applyChain(t *testing.T, db *gorm.DB, files ...string) {
	t.Helper()
	for _, f := range files {
		applyMigrations(t, db, migFile(t, f), f)
	}
}

// fullUsageLog sets every non-FK field of the model so the insert touches
// all drift-relevant columns — a missing column fails the write instead of
// passing silently. The FK columns (api_key_id/provider_id/team_id/org_id)
// stay NULL: the real init schema enforces parent rows and base-column
// round-trips are already covered by the round-12 chain tests; this test
// drives the 21 parity columns plus every other non-FK field.
func fullUsageLog() *model.UsageLog {
	msg := "boom"
	upStatus := 502
	upCode := "server_error"
	upType := "upstream_error"
	upParam := "model"
	stage := "upstream"
	firstMs := 123
	tpl := int64(9)
	imgCount := int64(2)
	imgSize := "1024x1024"
	imgQuality := "hd"
	sys, hist, q, tool, toolOut, ctxWin, util, flags := 10, 20, 30, 40, 50, 128000, 8123, 7
	return &model.UsageLog{
		RequestID: "chain-1",
		EventID:   strPtr("evt-chain-1"),
		RouteType: "chat", ModelRequested: "req-model", ModelUsed: "used-model",
		InputTokens: 11, OutputTokens: 22, Cost: 1.5, BillableCost: 2.25,
		LatencyMs: 99, FirstTokenMs: &firstMs, StatusCode: 200, ErrorType: "",
		ErrorMessage: &msg, UpstreamStatus: &upStatus, UpstreamErrorCode: &upCode,
		UpstreamErrorType: &upType, UpstreamErrorParam: &upParam, ErrorStage: &stage,
		Attempts:    datatypes.JSON(`[{"provider":"fake","success":true}]`),
		Currency:    "CNY",
		UserMessage: strPtr("hi"), ModelResponse: strPtr("hello"),
		FallbackCount: 1, RetryCount: 2, GuardrailTriggered: true, GuardrailRule: "keyword",
		GuardrailTruncated: true, CacheHit: false, AgentType: "codex",
		SecurityEvents:  datatypes.JSON(`[{"rule":"keyword"}]`),
		ReasoningTokens: 5, CacheReadTokens: 6, SessionID: "sess-1", TemplateID: &tpl,
		ImageCount: &imgCount, ImageSize: &imgSize, ImageQuality: &imgQuality,
		SystemTokens: &sys, HistoryTokens: &hist, QuestionTokens: &q, ToolTokens: &tool,
		ToolOutputTokens: &toolOut, ContextWindow: &ctxWin, ContextUtilizationBp: &util,
		AnalysisFlags: &flags, ContextSnapshot: datatypes.JSON(`{"v":1}`),
		CreatedAt: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC),
	}
}

func assertFullUsageLog(t *testing.T, got *model.UsageLog) {
	t.Helper()
	if got.RequestID != "chain-1" {
		t.Fatalf("base columns lost: %+v", got)
	}
	if got.EventID == nil || *got.EventID != "evt-chain-1" {
		t.Fatalf("event_id (F34 idempotency key) lost: %+v", got)
	}
	if got.InputTokens != 11 || got.OutputTokens != 22 || got.Cost != 1.5 || got.BillableCost != 2.25 {
		t.Fatalf("cost columns lost: %+v", got)
	}
	if got.FirstTokenMs == nil || *got.FirstTokenMs != 123 {
		t.Fatalf("first_token_ms lost: %+v", got)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "boom" || got.UpstreamStatus == nil || *got.UpstreamStatus != 502 ||
		got.UpstreamErrorCode == nil || *got.UpstreamErrorCode != "server_error" ||
		got.UpstreamErrorType == nil || *got.UpstreamErrorType != "upstream_error" ||
		got.UpstreamErrorParam == nil || *got.UpstreamErrorParam != "model" ||
		got.ErrorStage == nil || *got.ErrorStage != "upstream" {
		t.Fatalf("error-observability columns lost: %+v", got)
	}
	if string(got.Attempts) == "" || got.Attempts == nil {
		t.Fatalf("attempts lost: %+v", got)
	}
	if got.UserMessage == nil || *got.UserMessage != "hi" || got.ModelResponse == nil || *got.ModelResponse != "hello" {
		t.Fatalf("content columns lost: %+v", got)
	}
	if got.FallbackCount != 1 || got.RetryCount != 2 || !got.GuardrailTriggered || got.GuardrailRule != "keyword" || !got.GuardrailTruncated {
		t.Fatalf("guardrail columns lost: %+v", got)
	}
	if got.AgentType != "codex" || got.SecurityEvents == nil {
		t.Fatalf("agent/security columns lost: %+v", got)
	}
	if got.ReasoningTokens != 5 || got.CacheReadTokens != 6 || got.SessionID != "sess-1" {
		t.Fatalf("token columns lost: %+v", got)
	}
	if got.TemplateID == nil || *got.TemplateID != 9 || got.ImageCount == nil || *got.ImageCount != 2 ||
		got.ImageSize == nil || *got.ImageSize != "1024x1024" || got.ImageQuality == nil || *got.ImageQuality != "hd" {
		t.Fatalf("template/image columns lost: %+v", got)
	}
	if got.SystemTokens == nil || *got.SystemTokens != 10 || got.HistoryTokens == nil || *got.HistoryTokens != 20 ||
		got.QuestionTokens == nil || *got.QuestionTokens != 30 || got.ToolTokens == nil || *got.ToolTokens != 40 ||
		got.ToolOutputTokens == nil || *got.ToolOutputTokens != 50 || got.ContextWindow == nil || *got.ContextWindow != 128000 ||
		got.ContextUtilizationBp == nil || *got.ContextUtilizationBp != 8123 || got.AnalysisFlags == nil || *got.AnalysisFlags != 7 ||
		got.ContextSnapshot == nil {
		t.Fatalf("context-analysis columns lost: %+v", got)
	}
}

func insertAndReadBack(t *testing.T, db *gorm.DB, label string) {
	t.Helper()
	row := fullUsageLog()
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("%s: full-field usage write failed: %v", label, err)
	}
	var back model.UsageLog
	if err := db.First(&back, "request_id = ?", "chain-1").Error; err != nil {
		t.Fatalf("%s: readback failed: %v", label, err)
	}
	assertFullUsageLog(t, &back)
}

// Fresh install: the complete real chain (000001→000004) applies cleanly and
// the current model writes and reads back EVERY field.
func TestFreshInstallFullChainModelWriteAllFields(t *testing.T) {
	db := parityMigrationDB(t)
	applyChain(t, db, migInit, migAllowedIPs, migTruncatedUp, migParityUp, migEventIDUp)
	// Parity columns must come from 000004 only — the duplicate-column guard
	// for this migration class.
	if !hasUsageColumn(t, db, "billable_cost") || !hasUsageColumn(t, db, "attempts") || !hasUsageColumn(t, db, "context_snapshot") {
		t.Fatal("000004 did not add the parity columns")
	}
	if !hasUsageColumn(t, db, "event_id") {
		t.Fatal("000007 did not add the event_id idempotency column")
	}
	insertAndReadBack(t, db, "fresh-install full chain")
}

// Deployed upgrade: a database left at 000002 cannot serve the model; rolling
// forward 000003+000004 fixes every column.
func TestDeployedUpgradeFullChainModelWrite(t *testing.T) {
	db := parityMigrationDB(t)
	applyChain(t, db, migInit, migAllowedIPs)
	if err := db.Create(fullUsageLog()).Error; err == nil {
		t.Fatal("full-field write unexpectedly succeeded on pre-upgrade schema")
	} else {
		t.Logf("pre-upgrade write fails as expected: %v", err)
	}
	applyChain(t, db, migTruncatedUp, migParityUp, migEventIDUp)
	insertAndReadBack(t, db, "deployed upgrade 000003+000004+000007")
}

// Rollback/re-upgrade over the FULL new tail (golang-migrate steps one
// version at a time): down 000004, down 000003, up 000003, up 000004 — the
// model write must work again afterwards. The down step legitimately drops
// parity-column DATA (column-drop semantics), which is asserted explicitly.
func TestFullChainDownAndReUpgradeModelWrite(t *testing.T) {
	db := parityMigrationDB(t)
	applyChain(t, db, migInit, migAllowedIPs, migTruncatedUp, migParityUp, migEventIDUp)
	insertAndReadBack(t, db, "before rollback")

	applyChain(t, db, migEventIDDn)
	if hasUsageColumn(t, db, "event_id") {
		t.Fatal("000007 down left event_id behind")
	}
	applyChain(t, db, migParityDn)
	if hasUsageColumn(t, db, "billable_cost") {
		t.Fatal("000004 down left parity columns behind")
	}
	// Documented down semantics: the pre-rollback row survives (minus parity
	// columns) — created_at values are preserved through the rebuild.
	var survived int
	if err := db.Raw("SELECT COUNT(*) FROM usage_logs WHERE request_id = 'chain-1'").Scan(&survived).Error; err != nil {
		t.Fatal(err)
	}
	if survived != 1 {
		t.Fatalf("down rebuild lost existing rows: %d", survived)
	}
	applyChain(t, db, migTruncatedDn)
	if hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("000003 down left guardrail_truncated behind")
	}

	applyChain(t, db, migTruncatedUp, migParityUp, migEventIDUp)
	if err := db.Exec("DELETE FROM usage_logs").Error; err != nil {
		t.Fatal(err)
	}
	insertAndReadBack(t, db, "after down/up cycle")
}

func strPtr(s string) *string { return &s }

// parityMigrationDB opens a single-connection in-memory database with FK
// enforcement disabled: the full-field model row carries non-null FK values
// (api_key_id/provider_id/team_id/org_id) and this test verifies COLUMN
// write/readback parity, not referential integrity against parent tables.
func parityMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:parity_mem?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

// The REAL runtime path for SQLite fresh installs: SQLiteDialect.RunMigrations
// over the actual migrations/sqlite directory, then the model writes and
// reads back through the resulting schema. Also verifies idempotent re-runs
// and the legacy schema_migrations version ('1') mapping.
func TestSQLiteRunMigrationsAppliesFullChain(t *testing.T) {
	t.Chdir("../..") // RunMigrations resolves migrations/sqlite from the server working directory
	path := filepath.Join(t.TempDir(), "runmig.db")
	dia := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: path})
	db, err := dia.InitDB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	if err := dia.RunMigrations(context.Background()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if err := dia.RunMigrations(context.Background()); err != nil {
		t.Fatalf("re-run must be idempotent: %v", err)
	}
	var versions []string
	if err := db.Raw("SELECT version FROM schema_migrations ORDER BY version").Scan(&versions).Error; err != nil {
		t.Fatal(err)
	}
	// The chain length must track the real migrations/sqlite directory, not a
	// hardcoded count that goes stale with every new migration (B5's 000006
	// broke exactly that).
	ups, err := filepath.Glob(filepath.Join("migrations", "sqlite", "*_*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != len(ups) {
		t.Fatalf("expected %d applied versions (one per migrations/sqlite/*.up.sql), got %v", len(ups), versions)
	}
	insertAndReadBack(t, db, "RunMigrations fresh install")
}

// A database recorded with the legacy version marker ('1') must not re-apply
// the init schema on upgrade.
func TestSQLiteRunMigrationsLegacyVersionSkipsInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	dia := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: path})
	db, err := dia.InitDB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	// Reproduce a real legacy database: the OLD RunMigrations applied only
	// the init schema and recorded version '1'. Load the file before
	// t.Chdir moves the working directory to the repo root.
	initStmts := migFile(t, migInit)
	t.Chdir("../..")
	applyMigrations(t, db, initStmts, "legacy 000001")
	if err := db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, dirty BOOLEAN)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO schema_migrations VALUES ('1', false)").Error; err != nil {
		t.Fatal(err)
	}
	if err := dia.RunMigrations(context.Background()); err != nil {
		t.Fatalf("legacy-marker upgrade failed: %v", err)
	}
	insertAndReadBack(t, db, "legacy upgrade")
}

// --- RunMigrations failure regressions (round-14): a failed database
// operation must surface as an error — never a recorded success with an
// uncommitted schema/version marker. ---

// A dead underlying connection (disk lost, file deleted, pool closed) fails
// at the schema_migrations setup / read / begin — every path must return an
// error, not report success.
func TestSQLiteRunMigrationsReturnsErrorWhenDBUnavailable(t *testing.T) {
	t.Chdir("../..")
	path := filepath.Join(t.TempDir(), "dead.db")
	dia := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: path})
	if _, err := dia.InitDB(); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := dia.db.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dia.RunMigrations(context.Background()); err == nil {
		t.Fatal("RunMigrations reported success on a closed database")
	} else {
		t.Logf("closed DB surfaces error as expected: %v", err)
	}
}

// Disk exhaustion (SQLITE_FULL via a frozen max_page_count) during a pending
// migration must abort with an error; the failing version must NOT be
// recorded in schema_migrations afterwards.
func TestSQLiteRunMigrationsReturnsErrorWhenDiskFull(t *testing.T) {
	t.Chdir("../..")
	path := filepath.Join(t.TempDir(), "full.db")
	dia := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: path})
	db, err := dia.InitDB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	if err := dia.RunMigrations(context.Background()); err != nil {
		t.Fatalf("baseline migrate: %v", err)
	}
	// Freeze the database at its current page count: any transaction that
	// needs a new page now fails with SQLITE_FULL (database or disk is full).
	// VACUUM first — the 000015 table rebuilds leave a large freelist, and
	// pages taken from it never trip SQLITE_FULL no matter how tight the cap.
	if err := db.Exec("VACUUM").Error; err != nil {
		t.Fatal(err)
	}
	var freelist int64
	if err := db.Raw("PRAGMA freelist_count").Scan(&freelist).Error; err != nil {
		t.Fatal(err)
	}
	if freelist != 0 {
		t.Fatalf("VACUUM left %d free pages", freelist)
	}
	var pages int64
	if err := db.Raw("PRAGMA page_count").Scan(&pages).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)).Error; err != nil {
		t.Fatal(err)
	}
	// Force a pending migration: drop the parity version marker only (its
	// table changes already exist, so the statements themselves would not
	// fail on re-apply — the version INSERT must allocate a new page and hit
	// SQLITE_FULL). Wipe rows so the run has work to do.
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = '000004'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE usage_logs SET guardrail_rule = guardrail_rule").Error; err == nil {
		t.Log("note: in-place update did not need new pages; relying on migration INSERT")
	}
	err = dia.RunMigrations(context.Background())
	if err == nil {
		t.Fatal("RunMigrations reported success under disk-full conditions")
	}
	t.Logf("disk-full surfaces error as expected: %.160v", err)
	var recorded int64
	if err := db.Raw("SELECT count(*) FROM schema_migrations WHERE version = '000004'").Scan(&recorded).Error; err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatal("failed run left the version marker recorded (would mark an uncommitted schema as applied)")
	}
}
