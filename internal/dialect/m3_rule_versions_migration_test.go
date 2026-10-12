package dialect

// M3-A rule-versions migration regressions. These execute the ACTUAL
// migration files in the ACTUAL order — the same files golang-migrate runs
// in production:
//
//   - fresh install: full chain through the new version applies cleanly;
//   - deployed upgrade: rules that predate versioning get version=1 and a
//     'migrate' snapshot (the backfill), and the backfill statement itself
//     is re-run safe (NOT EXISTS guard);
//   - rollback/re-upgrade: down removes the artifacts, up restores them and
//     regenerates the backfill;
//   - gorm history-model parity: rows written by the migration read back
//     through the model.
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

	"github.com/crosslink/internal/guardrail"
	gomysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type m3MigDB struct {
	name     string
	db       *gorm.DB
	chainDir string
	prevTail int // last version of the previously released chain
	newVer   int // this batch's migration version
	newSlug  string
}

func (p m3MigDB) applyUpTo(t *testing.T, maxVer int) {
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

func (p m3MigDB) applyOne(t *testing.T, down bool) {
	t.Helper()
	suffix := ".up.sql"
	if down {
		suffix = ".down.sql"
	}
	applyMigrations(t, p.db, migFile(t, fmt.Sprintf("%s/%06d_%s%s", p.chainDir, p.newVer, p.newSlug, suffix)),
		fmt.Sprintf("%s %04d %s", p.name, p.newVer, suffix))
}

func m3Count(t *testing.T, db *gorm.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(query).Scan(&n).Error; err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func m3SeedRule(t *testing.T, db *gorm.DB, name, keyword string) {
	t.Helper()
	cfg := fmt.Sprintf(`{"blocklist":["%s"]}`, keyword)
	stmt := fmt.Sprintf(`INSERT INTO guardrail_rules (org_id, name, type, direction, enabled, config, severity, action)
		VALUES (NULL, '%s', 'keyword_filter', 'both', true, '%s', 'medium', 'block')`, name, cfg)
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("seed rule: %v", err)
	}
}

func m3VersionColumnPresent(t *testing.T, p m3MigDB) bool {
	t.Helper()
	switch p.name {
	case "sqlite":
		var cols int64
		p.db.Raw(`SELECT COUNT(*) FROM pragma_table_info('guardrail_rules') WHERE name='version'`).Scan(&cols)
		return cols != 0
	case "postgres":
		var cols int64
		p.db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='guardrail_rules' AND column_name='version'`).Scan(&cols)
		return cols != 0
	case "mysql":
		var cols int64
		p.db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='guardrail_rules' AND column_name='version' AND table_schema=DATABASE()`).Scan(&cols)
		return cols != 0
	}
	return false
}

// m3Dialects returns one CLEAN m3MigDB per reachable dialect. SQLite is
// always present (in-process, fresh per call).
func m3Dialects(t *testing.T) []m3MigDB {
	t.Helper()
	var out []m3MigDB

	out = append(out, m3MigDB{name: "sqlite", db: freshMigrationDB(t), chainDir: "../../migrations/sqlite", prevTail: 4, newVer: 5, newSlug: "rule_versions"})

	pgDSN := os.Getenv("PG_TEST_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://crosslink:crosslink_test@localhost:15433/crosslink_test_pg?sslmode=disable"
	}
	if db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		db.Exec("DROP SCHEMA public CASCADE")
		db.Exec("CREATE SCHEMA public")
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		out = append(out, m3MigDB{name: "postgres", db: db, chainDir: "../../migrations/postgres", prevTail: 79, newVer: 80, newSlug: "rule_versions"})
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
		out = append(out, m3MigDB{name: "mysql", db: db, chainDir: "../../migrations/mysql", prevTail: 4, newVer: 5, newSlug: "rule_versions"})
	} else {
		t.Logf("mysql test instance unreachable (%v) — mysql subtests will skip", err)
	}
	return out
}

func TestM3RuleVersionsMigrationChain(t *testing.T) {
	for _, dia := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dia+"/fresh_install", func(t *testing.T) {
			for _, p := range m3Dialects(t) {
				if p.name != dia {
					continue
				}
				// the init schema must NOT contain the version artifacts
				// (duplicate-column fault class from round-12)
				p.applyUpTo(t, p.prevTail)
				if p.name == "sqlite" && m3VersionColumnPresent(t, p) {
					t.Fatal("previous chain already contains guardrail_rules.version — fresh installs would fail with duplicate column")
				}
				p.applyOne(t, false)
				if !m3VersionColumnPresent(t, p) {
					t.Fatal("version column missing after migration")
				}
				if n := m3Count(t, p.db, `SELECT COUNT(*) FROM guardrail_rule_versions`); n != 0 {
					t.Fatalf("fresh install must not backfill anything (no rules yet), got %d rows", n)
				}
				if err := p.db.Raw(`SELECT rule_version FROM guardrail_alert_logs LIMIT 0`).Error; err != nil {
					t.Fatalf("alert_logs.rule_version missing: %v", err)
				}
				t.Logf("%s: fresh install full chain OK (empty backfill, version + alert columns present)", p.name)
			}
		})

		t.Run(dia+"/deployed_upgrade_backfill", func(t *testing.T) {
			for _, p := range m3Dialects(t) {
				if p.name != dia {
					continue
				}
				p.applyUpTo(t, p.prevTail)
				m3SeedRule(t, p.db, "pre-existing one", "OLDKEYA")
				m3SeedRule(t, p.db, "pre-existing two", "OLDKEYB")

				p.applyOne(t, false) // the new migration

				if n := m3Count(t, p.db, `SELECT COUNT(*) FROM guardrail_rules WHERE version = 1`); n != 2 {
					t.Fatalf("both pre-existing rules must be at version 1, got %d", n)
				}
				if n := m3Count(t, p.db, `SELECT COUNT(*) FROM guardrail_rule_versions WHERE change_kind = 'migrate'`); n != 2 {
					t.Fatalf("backfill must snapshot both rules, got %d", n)
				}
				// gorm model parity: migration-written rows read back with the
				// original config (works across JSONB/JSON/TEXT dialects;
				// a raw LIKE is not portable to JSONB)
				var hist guardrail.GuardrailRuleVersion
				if err := p.db.Where("change_kind = 'migrate'").Order("rule_id ASC").First(&hist).Error; err != nil {
					t.Fatalf("history model round-trip: %v", err)
				}
				if hist.Version != 1 || !strings.Contains(string(hist.Config), "OLDKEYA") {
					t.Fatalf("history row content wrong: %+v", hist)
				}
				// re-run safety of the BACKFILL: golang-migrate never re-applies
				// a migration file (and the ALTERs are intentionally not
				// idempotent, matching repo migration conventions), but the
				// backfill INSERT carries a NOT EXISTS guard — replaying only
				// that statement must not duplicate history rows.
				stmts := migFile(t, fmt.Sprintf("%s/%06d_%s.up.sql", p.chainDir, p.newVer, p.newSlug))
				backfills := 0
				for _, s := range stmts {
					if strings.Contains(s, "INSERT INTO guardrail_rule_versions") {
						applyMigrations(t, p.db, []string{s}, "backfill replay")
						backfills++
					}
				}
				if backfills == 0 {
					t.Fatal("no backfill INSERT found in migration file — test is not testing the guard")
				}
				if n := m3Count(t, p.db, `SELECT COUNT(*) FROM guardrail_rule_versions`); n != 2 {
					t.Fatalf("replaying the backfill must not duplicate history rows, got %d", n)
				}
				t.Logf("%s: deployed upgrade OK — 2 rules backfilled (version=1, migrate snapshots, original configs, model parity, backfill replay safe)", p.name)
			}
		})

		t.Run(dia+"/down_and_reup", func(t *testing.T) {
			for _, p := range m3Dialects(t) {
				if p.name != dia {
					continue
				}
				p.applyUpTo(t, p.prevTail)
				m3SeedRule(t, p.db, "pre-existing one", "OLDKEYA")
				p.applyOne(t, false)

				p.applyOne(t, true) // down: artifacts removed
				if m3VersionColumnPresent(t, p) {
					t.Fatal("version column must be dropped on down")
				}
				if n := m3Count(t, p.db, `SELECT COUNT(*) FROM guardrail_rules`); n != 1 {
					t.Fatalf("down must keep the rule rows, got %d", n)
				}

				p.applyOne(t, false) // up again: artifacts restored, backfill regenerated
				if n := m3Count(t, p.db, `SELECT COUNT(*) FROM guardrail_rule_versions WHERE change_kind = 'migrate'`); n != 1 {
					t.Fatalf("re-upgrade must regenerate the backfill, got %d", n)
				}
				t.Logf("%s: down/up OK — artifacts removed, rules kept, backfill regenerated", p.name)
			}
		})
	}
}
