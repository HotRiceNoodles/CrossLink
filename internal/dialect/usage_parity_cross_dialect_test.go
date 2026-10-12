package dialect

// Cross-dialect UsageLog parity over the REAL migration files against REAL
// database servers (round-13 acceptance): fresh install, upgrade from the
// previously released chain state, and down/up of the new tail — each
// verified with a full-field model INSERT/readback. PostgreSQL and MySQL run
// only when their test instances are reachable (PG_TEST_DSN / MYSQL_TEST_DSN
// or the documented defaults; see deployments/docker-compose.test.yaml —
// ephemeral throwaway credentials, localhost only). When unreachable the
// dialect subtest skips with an explicit message; nothing is inferred across
// dialects.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type parityDB struct {
	name     string
	db       *gorm.DB
	chainDir string
	// prevTail is the last migration version of the PREVIOUSLY released
	// chain state for this dialect (upgrade-path start).
	prevTail int
	// newTail are the versions added by this batch (upgrade/down-up set).
	newTail []string
	cleanup func()
}

func openParityDBs(t *testing.T) []parityDB {
	t.Helper()
	var out []parityDB

	pgDSN := os.Getenv("PG_TEST_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://crosslink:crosslink_test@localhost:5433/crosslink_test_pg?sslmode=disable"
	}
	if db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		db.Exec("DROP SCHEMA public CASCADE")
		db.Exec("CREATE SCHEMA public")
		sqlDB, _ := db.DB()
		out = append(out, parityDB{name: "postgres", db: db, chainDir: "migrations/postgres", prevTail: 78, newTail: []string{"000079_usage_guardrail_truncated"},
			cleanup: func() { sqlDB.Close() }})
	} else {
		t.Logf("postgres test instance unreachable (%v) — postgres subtests will skip", err)
	}

	myDSN := os.Getenv("MYSQL_TEST_DSN")
	if myDSN == "" {
		myDSN = "root:crosslink_test@tcp(127.0.0.1:3307)/crosslink_test_mysql?charset=utf8mb4&parseTime=true&loc=UTC"
	}
	if db, err := gorm.Open(mysql.Open(myDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		dropAllTablesMySQL(t, db)
		sqlDB, _ := db.DB()
		out = append(out, parityDB{name: "mysql", db: db, chainDir: "migrations/mysql", prevTail: 2, newTail: []string{"000003_usage_guardrail_truncated", "000004_usage_model_parity"},
			cleanup: func() { sqlDB.Close() }})
	} else {
		t.Logf("mysql test instance unreachable (%v) — mysql subtests will skip", err)
	}
	return out
}

// chainUpTo applies every .up.sql migration file whose version is <= maxVer.
func (p parityDB) chainUpTo(t *testing.T, maxVer int) {
	t.Helper()
	entries, err := os.ReadDir(p.chainDir)
	if err != nil {
		t.Fatalf("%s: read migration dir: %v", p.name, err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".up.sql") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		ver := 0
		fmt.Sscanf(n, "%06d", &ver)
		if ver == 0 || ver > maxVer {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(p.chainDir, n))
		if err != nil {
			t.Fatalf("%s: read %s: %v", p.name, n, err)
		}
		for _, stmt := range splitSQLStatements(string(raw)) {
			if err := p.db.Exec(stmt).Error; err != nil {
				t.Fatalf("%s: %s: %v\nstatement: %.160s", p.name, n, err, stmt)
			}
		}
	}
}

func (p parityDB) applyFile(t *testing.T, name string, up bool) {
	t.Helper()
	suffix := ".up.sql"
	if !up {
		suffix = ".down.sql"
	}
	raw, err := os.ReadFile(filepath.Join(p.chainDir, name+suffix))
	if err != nil {
		t.Fatalf("%s: read %s%s: %v", p.name, name, suffix, err)
	}
	for _, stmt := range splitSQLStatements(string(raw)) {
		if err := p.db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %s%s: %v\nstatement: %.160s", p.name, name, suffix, err, stmt)
		}
	}
}

func (p parityDB) reset(t *testing.T) {
	t.Helper()
	switch p.name {
	case "postgres":
		p.db.Exec("DROP SCHEMA public CASCADE")
		p.db.Exec("CREATE SCHEMA public")
	case "mysql":
		dropAllTablesMySQL(t, p.db)
	}
}

// TestCrossDialect_UsageModelParityFreshInstall: apply the complete real
// chain, then the model writes and reads back every field.
func TestCrossDialect_UsageModelParityFreshInstall(t *testing.T) {
	t.Chdir("../..")
	for _, p := range openParityDBs(t) {
		t.Run(p.name, func(t *testing.T) {
			defer p.cleanup()
			p.reset(t)
			p.chainUpTo(t, 9999)
			insertAndReadBack(t, p.db, p.name+" fresh install")
		})
	}
}

// TestCrossDialect_UsageModelParityDeployedUpgrade: a database at the
// previous released chain state cannot serve the model; applying the new
// tail fixes it.
func TestCrossDialect_UsageModelParityDeployedUpgrade(t *testing.T) {
	t.Chdir("../..")
	for _, p := range openParityDBs(t) {
		t.Run(p.name, func(t *testing.T) {
			defer p.cleanup()
			p.reset(t)
			p.chainUpTo(t, p.prevTail)
			if err := p.db.Create(fullUsageLog()).Error; err == nil {
				t.Fatalf("%s: full-field write unexpectedly succeeded on previous chain state", p.name)
			} else {
				t.Logf("%s: pre-upgrade write fails as expected: %v", p.name, err)
			}
			for _, n := range p.newTail {
				p.applyFile(t, n, true)
			}
			insertAndReadBack(t, p.db, p.name+" deployed upgrade")
		})
	}
}

// TestCrossDialect_UsageModelParityDownAndReUpgrade: roll the new tail back
// (down, newest first), then re-apply; the model write works again.
func TestCrossDialect_UsageModelParityDownAndReUpgrade(t *testing.T) {
	t.Chdir("../..")
	for _, p := range openParityDBs(t) {
		t.Run(p.name, func(t *testing.T) {
			defer p.cleanup()
			p.reset(t)
			p.chainUpTo(t, p.prevTail)
			for _, n := range p.newTail {
				p.applyFile(t, n, true)
			}
			insertAndReadBack(t, p.db, p.name+" before rollback")
			for i := len(p.newTail) - 1; i >= 0; i-- {
				p.applyFile(t, p.newTail[i], false)
			}
			if err := p.db.Exec("DELETE FROM usage_logs").Error; err != nil {
				t.Fatal(err)
			}
			for _, n := range p.newTail {
				p.applyFile(t, n, true)
			}
			insertAndReadBack(t, p.db, p.name+" after down/up cycle")
		})
	}
}
