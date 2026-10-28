package dialect

// guard_tables migration regressions (crosslink-guard M1 Step 3). These
// execute the ACTUAL migration files in the ACTUAL order per dialect chain:
//
//   - fresh install: full chain through the new version applies cleanly
//     (NOTE: chain numbering diverged across dialects — mysql/sqlite were
//     rebased onto squashed baselines, postgres kept full history; the new
//     guard tables pair appends per chain: mysql 10, sqlite 11, pg 86);
//   - gorm model parity: rows written through the guard models read back;
//   - rollback/re-upgrade: down drops the three tables, up restores them;
//   - (org_id, trace_id) uniqueness enforces the tenant-scoped trace key.
//
// SQLite runs in-process. Postgres/MySQL run only when reachable
// (PG_TEST_DSN / MYSQL_TEST_DSN or the defaults below) — throwaway test
// containers, never production.

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	"gorm.io/datatypes"
	gomysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type guardMigDB struct {
	name     string
	db       *gorm.DB
	chainDir string
	newVer   int
	newSlug  string
}

// applyChainUp runs every .up.sql with version <= maxVer in chain order.
func (p guardMigDB) applyChainUp(t *testing.T, maxVer int) {
	t.Helper()
	entries, err := os.ReadDir(p.chainDir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".up.sql") {
			continue
		}
		ver, perr := strconv.Atoi(strings.SplitN(n, "_", 2)[0])
		if perr != nil {
			continue
		}
		if ver <= maxVer {
			files = append(files, n)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		applyMigrations(t, p.db, migFile(t, p.chainDir+"/"+f), p.name+" "+f)
	}
}

func (p guardMigDB) applyOne(t *testing.T, down bool) {
	t.Helper()
	suffix := ".up.sql"
	if down {
		suffix = ".down.sql"
	}
	applyMigrations(t, p.db, migFile(t, fmt.Sprintf("%s/%06d_%s%s", p.chainDir, p.newVer, p.newSlug, suffix)),
		fmt.Sprintf("%s %04d %s", p.name, p.newVer, suffix))
}

// tableGone reports whether the table is absent — a SELECT against a missing
// table errors in every dialect.
func (p guardMigDB) tableGone(t *testing.T, table string) bool {
	t.Helper()
	var n int64
	return p.db.Raw("SELECT COUNT(*) FROM "+table).Scan(&n).Error != nil
}

func guardMigDialects(t *testing.T) []guardMigDB {
	t.Helper()
	var out []guardMigDB

	out = append(out, guardMigDB{name: "sqlite", db: freshMigrationDB(t), chainDir: "../../migrations/sqlite", newVer: 11, newSlug: "guard_tables"})

	pgDSN := os.Getenv("PG_TEST_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://crosslink:crosslink_test@localhost:15433/crosslink_test_pg?sslmode=disable"
	}
	if db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		db.Exec("DROP SCHEMA public CASCADE")
		db.Exec("CREATE SCHEMA public")
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		out = append(out, guardMigDB{name: "postgres", db: db, chainDir: "../../migrations/postgres", newVer: 86, newSlug: "guard_tables"})
	} else {
		t.Logf("postgres test instance unreachable (%v) — postgres subtests will skip", err)
	}

	myDSN := os.Getenv("MYSQL_TEST_DSN")
	if myDSN == "" {
		myDSN = "root:crosslink_test@tcp(127.0.0.1:13307)/crosslink_test_mysql?charset=utf8mb4&parseTime=true&loc=UTC"
	}
	if db, err := gorm.Open(gomysql.Open(myDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		dropAllTablesMySQL(t, db)
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		out = append(out, guardMigDB{name: "mysql", db: db, chainDir: "../../migrations/mysql", newVer: 10, newSlug: "guard_tables"})
	} else {
		t.Logf("mysql test instance unreachable (%v) — mysql subtests will skip", err)
	}
	return out
}

func TestGuardTablesMigration(t *testing.T) {
	for _, p := range guardMigDialects(t) {
		t.Run(p.name, func(t *testing.T) {
			// Fresh install: the complete chain through the new pair.
			p.applyChainUp(t, p.newVer)

			// Model parity: write through the gorm models, read back.
			tok := &model.GuardIngestToken{OrgID: 7, Name: "edge-mirror", TokenHash: "a" + strings.Repeat("0", 63), KeyPrefix: "cg-test", Status: 1}
			if err := p.db.Create(tok).Error; err != nil {
				t.Fatalf("insert token: %v", err)
			}
			var gotTok model.GuardIngestToken
			if err := p.db.Where("token_hash = ?", tok.TokenHash).First(&gotTok).Error; err != nil {
				t.Fatalf("read back token: %v", err)
			}
			if gotTok.OrgID != 7 || gotTok.KeyPrefix != "cg-test" {
				t.Fatalf("token parity: %+v", gotTok)
			}

			ev := &model.GuardEvent{TraceID: "tr-1", OrgID: 7, RuleID: 3, EngineType: "keyword_filter",
				Direction: "request", Action: "would_block", Severity: "high", ContentSHA256: "b" + strings.Repeat("0", 63),
				Metadata: datatypes.JSON([]byte(`{"channel":"mirror"}`))}
			if err := p.db.Create(ev).Error; err != nil {
				t.Fatalf("insert event: %v", err)
			}
			var gotEv model.GuardEvent
			if err := p.db.First(&gotEv, ev.ID).Error; err != nil {
				t.Fatalf("read back event: %v", err)
			}
			if gotEv.Action != "would_block" || string(gotEv.Metadata) == "" {
				t.Fatalf("event parity: %+v metadata=%s", gotEv, gotEv.Metadata)
			}

			tr := &model.GuardTrace{TraceID: "tr-1", OrgID: 7}
			if err := p.db.Create(tr).Error; err != nil {
				t.Fatalf("insert trace: %v", err)
			}

			// Tenant-scoped trace key: same trace_id under another org is a
			// different trace; the same (org, trace) must collide.
			if err := p.db.Create(&model.GuardTrace{TraceID: "tr-1", OrgID: 8}).Error; err != nil {
				t.Fatalf("cross-org same trace_id should be allowed: %v", err)
			}
			if err := p.db.Create(&model.GuardTrace{TraceID: "tr-1", OrgID: 7}).Error; err == nil {
				t.Fatal("duplicate (org_id, trace_id) must be rejected")
			}

			// Rollback: down drops all three tables...
			p.applyOne(t, true)
			for _, table := range []string{"guard_traces", "guard_events", "guard_ingest_tokens"} {
				if !p.tableGone(t, table) {
					t.Fatalf("%s still present after down", table)
				}
			}
			// ...and re-upgrade restores them with parity intact.
			p.applyOne(t, false)
			if err := p.db.Create(&model.GuardEvent{TraceID: "tr-2", OrgID: 7, Direction: "response", Action: "log",
				Metadata: datatypes.JSON([]byte("{}"))}).Error; err != nil {
				t.Fatalf("insert event after re-up: %v", err)
			}
		})
	}
}

// TestSqliteGuardrailRulesDatetimeRoundTrip 覆盖 000012（guardrail_rules
// 时间列 TEXT→DATETIME 重建）：全链后插行留数据、down 恢复 TEXT 形态且行
// 保留、up 再建后 DATETIME 列可被 gorm 全行扫描（LoadRules 修复的回归面）。
func TestSqliteGuardrailRulesDatetimeRoundTrip(t *testing.T) {
	db := freshMigrationDB(t)
	entries, _ := os.ReadDir("../../migrations/sqlite")
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		applyMigrations(t, db, migFile(t, "../../migrations/sqlite/"+f), f)
	}

	db.Exec(`INSERT INTO guardrail_rules (name, type, direction, enabled, config, severity, action, org_id, version, created_at, updated_at)
		VALUES ('rt', 'keyword_filter', 'both', 1, '{}', 'high', 'block', NULL, 3, datetime('now'), datetime('now'))`)

	colType := func() string {
		var typ string
		db.Raw(`SELECT type FROM pragma_table_info('guardrail_rules') WHERE name = 'created_at'`).Scan(&typ)
		return typ
	}
	if colType() != "DATETIME" {
		t.Fatalf("after up: created_at type = %q", colType())
	}

	applyMigrations(t, db, migFile(t, "../../migrations/sqlite/000012_guardrail_rules_datetime.down.sql"), "000012 down")
	if colType() != "TEXT" {
		t.Fatalf("after down: created_at type = %q", colType())
	}
	var n int64
	db.Raw(`SELECT count(*) FROM guardrail_rules WHERE name = 'rt' AND version = 3`).Scan(&n)
	if n != 1 {
		t.Fatalf("row lost across rebuild: %d", n)
	}

	applyMigrations(t, db, migFile(t, "../../migrations/sqlite/000012_guardrail_rules_datetime.up.sql"), "000012 re-up")
	if colType() != "DATETIME" {
		t.Fatalf("after re-up: created_at type = %q", colType())
	}
	db.Raw(`SELECT count(*) FROM guardrail_rules WHERE name = 'rt' AND version = 3`).Scan(&n)
	if n != 1 {
		t.Fatalf("row lost across rebuild: %d", n)
	}
}
