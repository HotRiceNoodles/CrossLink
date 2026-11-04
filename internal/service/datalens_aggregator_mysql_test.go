//go:build integration

package service

// Live MySQL run of the same full aggregation lifecycle as the SQLite test:
// the MySQL-specific shapes — CONCAT() tool names, UPDATE ... JOIN (sub)
// distinct counts, DATE_FORMAT windows — only prove out against a real MySQL
// server. Skips unless the test instance is reachable (disposable mysql:8 on
// 3307; MYSQL_TEST_* env overrides, same defaults as the dialect parity
// tests in internal/dialect).

import (
	"context"
	"fmt"
	"testing"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/dialect"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/logger"
)

func TestAggregatorCycleMySQL(t *testing.T) {
	t.Chdir("../..") // migrations resolve from the repo root

	d := dialect.NewMySQLDialect(dialect.DBConfig{
		Driver:   "mysql",
		Host:     envOrDefault("MYSQL_TEST_HOST", "127.0.0.1"),
		Port:     envIntOrDefault("MYSQL_TEST_PORT", 3307),
		User:     envOrDefault("MYSQL_TEST_USER", "root"),
		Password: envOrDefault("MYSQL_TEST_PASSWORD", "crosslink_test"),
		DBName:   envOrDefault("MYSQL_TEST_DBNAME", "crosslink_test_mysql"),
	})
	db, err := d.InitDB()
	if err != nil {
		t.Skipf("mysql test instance unreachable (%v) — skipping", err)
	}
	db.Logger = logger.Default.LogMode(logger.Silent)

	// Clean slate: drop every table, FK checks off so order doesn't matter.
	db.Exec("SET FOREIGN_KEY_CHECKS = 0")
	var tables []string
	db.Raw("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables)
	for _, tbl := range tables {
		db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tbl))
	}
	db.Exec("SET FOREIGN_KEY_CHECKS = 1")
	require.NoError(t, d.RunMigrations(context.Background()))

	// usage_logs enforces FKs on org/key/provider/team — seed the parents.
	for _, stmt := range []string{
		"INSERT INTO organizations (id, name, display_name) VALUES (1, 'agg-test', 'agg-test')",
		"INSERT INTO providers (id, name, display_name, adapter_type, base_url, api_key) VALUES (2, 'agg-test', 'agg-test', 'openai_compatible', 'http://localhost', 'sk-test')",
		"INSERT INTO teams (id, name, display_name, org_id) VALUES (3, 'agg-test', 'agg-test', 1)",
		"INSERT INTO api_keys (id, name, key_hash, key_prefix) VALUES (7, 'agg-test', 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef', 'sk-aggt')",
	} {
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}

	ts := seedAggData(t, db)
	svc := NewDataLensAggregatorService(db, d, config.DataLensConfig{
		Agg: config.DataLensAggConfig{BackfillDays: 1},
	})
	assertAggregatorLifecycle(t, db, svc, ts)

	sqlDB, _ := db.DB()
	sqlDB.Close()
}
