package dialect

import (
	"os"
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Real migration-chain regressions for the guardrail_truncated column
// (round-12). These execute the ACTUAL migration files in the ACTUAL order —
// the same files golang-migrate runs in production — instead of deriving
// schemas from the model:
//
//   - fresh install: 000001 → 000002 → 000003 must apply cleanly (the
//     round-11 duplicate-column fault: the column may exist in exactly one
//     place — migration 000003 — never in the init schema),
//   - deployed upgrade: a database left at 000002 cannot write the column;
//     applying 000003 fixes it,
//   - rollback and re-upgrade: 000003.down removes the column, 000003.up
//     restores it,
//   - model parity: the gorm model round-trips the flag once the column is
//     present (AutoMigrate + drop-column simulates the deployed table).
//
// Verified on SQLite only. The Postgres (000079) and MySQL (000003)
// statements have NOT been executed against real servers in this environment
// and are explicitly UNVERIFIED — they follow the repo's migration
// conventions (000019/000076 forms) but no dialect pass is inferred from the
// SQLite run.

func migFile(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return splitSQLStatements(string(raw))
}

// splitSQLStatements splits a .sql file into executable statements. The
// migration files contain no triggers/procedures (verified), so splitting on
// statement-terminating semicolons followed by a line end is safe; comment
// lines travel with their statement, empty chunks are dropped.
func splitSQLStatements(sqlText string) []string {
	normalized := strings.ReplaceAll(sqlText, "\r\n", "\n")
	var stmts []string
	for _, chunk := range strings.Split(normalized, ";\n") {
		body := strings.TrimSpace(chunk)
		if body == "" {
			continue
		}
		// Drop chunks that are only comments/whitespace.
		onlyComments := true
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
				onlyComments = false
				break
			}
		}
		if !onlyComments {
			stmts = append(stmts, body)
		}
	}
	return stmts
}

func applyMigrations(t *testing.T, db *gorm.DB, stmts []string, label string) {
	t.Helper()
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("%s: %v\nstatement: %.200s", label, err, s)
		}
	}
}

func freshMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

const (
	migDir          = "../../migrations/sqlite/"
	migInit         = migDir + "000001_init_schema.up.sql"
	migAllowedIPs   = migDir + "000002_api_key_allowed_ips.up.sql"
	migTruncatedUp  = migDir + "000003_usage_guardrail_truncated.up.sql"
	migTruncatedDn  = migDir + "000003_usage_guardrail_truncated.down.sql"
	truncatedColumn = "guardrail_truncated"
)

// Fresh install applies the complete chain in order and must not fail — the
// duplicate-column fault class this round exists to prevent.
func TestFreshInstallMigrationChainAppliesCleanly(t *testing.T) {
	db := freshMigrationDB(t)
	initStmts := migFile(t, migInit)
	ipsStmts := migFile(t, migAllowedIPs)
	truncStmts := migFile(t, migTruncatedUp)

	applyMigrations(t, db, initStmts, "000001 init")
	// The init schema must NOT contain the column: migration 000003 is its
	// only executor. (Round-11 folded it into the init as well, which made
	// fresh installs fail with "duplicate column" on 000003.)
	if hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("init schema already contains guardrail_truncated — fresh installs would fail on 000003 (duplicate column)")
	}
	applyMigrations(t, db, ipsStmts, "000002 api_key_allowed_ips")
	if hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("guardrail_truncated appeared before 000003")
	}
	applyMigrations(t, db, truncStmts, "000003 guardrail_truncated")
	if !hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("000003 did not add guardrail_truncated")
	}
}

// A database deployed at 000002 (pre-column) cannot write the new column;
// applying 000003 upgrades it. Column semantics verified with an explicit
// column-list INSERT/SELECT (the historical init schema intentionally only
// covers the columns of its era; model-level parity is covered separately
// below).
func TestDeployedUpgradeMigrationAddsColumn(t *testing.T) {
	db := freshMigrationDB(t)
	applyMigrations(t, db, migFile(t, migInit), "000001 init")
	applyMigrations(t, db, migFile(t, migAllowedIPs), "000002 api_key_allowed_ips")

	ins := "INSERT INTO usage_logs (request_id, route_type, model_requested, model_used, status_code, guardrail_truncated) VALUES ('u1','chat','m','m',200,1)"
	if err := db.Exec(ins).Error; err == nil {
		t.Fatal("write with guardrail_truncated unexpectedly succeeded on pre-000003 schema")
	} else {
		t.Logf("pre-000003 write fails as expected: %v", err)
	}

	applyMigrations(t, db, migFile(t, migTruncatedUp), "000003 guardrail_truncated")
	if err := db.Exec(ins).Error; err != nil {
		t.Fatalf("post-000003 write failed: %v", err)
	}
	var flagged int
	if err := db.Raw("SELECT COUNT(*) FROM usage_logs WHERE request_id = 'u1' AND guardrail_truncated = 1").Scan(&flagged).Error; err != nil {
		t.Fatal(err)
	}
	if flagged != 1 {
		t.Fatalf("guardrail_truncated did not round-trip: %d rows flagged", flagged)
	}
}

// Rollback removes the column; re-running the up migration restores it
// (golang-migrate down/up cycle for 000003).
func TestTruncatedMigrationRollbackAndReUpgrade(t *testing.T) {
	db := freshMigrationDB(t)
	applyMigrations(t, db, migFile(t, migInit), "000001 init")
	applyMigrations(t, db, migFile(t, migAllowedIPs), "000002 api_key_allowed_ips")
	applyMigrations(t, db, migFile(t, migTruncatedUp), "000003 guardrail_truncated up")
	if !hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("up migration did not add the column")
	}

	applyMigrations(t, db, migFile(t, migTruncatedDn), "000003 guardrail_truncated down")
	if hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("down migration left guardrail_truncated behind")
	}

	applyMigrations(t, db, migFile(t, migTruncatedUp), "000003 guardrail_truncated re-up")
	if !hasUsageColumn(t, db, truncatedColumn) {
		t.Fatal("re-up migration did not restore the column")
	}
}

// Model parity: with the column present (deployed table simulated via
// AutoMigrate minus the column, then the real 000003 applied), full gorm
// writes round-trip the flag — usage logging cannot break on a missing
// column.
func TestUsageGuardrailTruncatedModelWriteAfterUpgrade(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE usage_logs DROP COLUMN guardrail_truncated").Error; err != nil {
		t.Fatalf("simulate deployed schema: %v", err)
	}

	if err := db.Create(&model.UsageLog{RequestID: "pre", RouteType: "chat", ModelRequested: "m", ModelUsed: "m", StatusCode: 200, Currency: "CNY"}).Error; err == nil {
		t.Fatal("model write unexpectedly succeeded without the column")
	} else {
		t.Logf("pre-migration model write fails as expected: %v", err)
	}

	applyMigrations(t, db, migFile(t, migTruncatedUp), "000003 guardrail_truncated")
	row := model.UsageLog{RequestID: "post", RouteType: "chat", ModelRequested: "m", ModelUsed: "m",
		StatusCode: 200, Currency: "CNY", GuardrailTriggered: true, GuardrailRule: "r", GuardrailTruncated: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("post-migration model write failed: %v", err)
	}
	var back model.UsageLog
	if err := db.First(&back, "request_id = ?", "post").Error; err != nil {
		t.Fatal(err)
	}
	if !back.GuardrailTruncated || !back.GuardrailTriggered || back.GuardrailRule != "r" {
		t.Fatalf("round-trip lost guardrail flags: %+v", back)
	}
}

func hasUsageColumn(t *testing.T, db *gorm.DB, want string) bool {
	t.Helper()
	rows, err := db.Raw("PRAGMA table_info(usage_logs)").Rows()
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	for _, c := range cols {
		if strings.EqualFold(c, want) {
			return true
		}
	}
	return false
}
