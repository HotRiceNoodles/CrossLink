//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAuditLogRepo_ILike_Postgres(t *testing.T) {
	db, dia, cleanup := setupAuditPGTestDB(t)
	defer cleanup()

	repo := NewAuditLogRepo(db, dia)

	now := time.Now()
	rows := []model.AuditLog{
		{UserID: 1, Username: "TestUser", Action: "create", ResourceType: "key", ResourceID: "1", ResourceName: "TestResource", Status: "success", CreatedAt: now},
		{UserID: 2, Username: "admin", Action: "delete", ResourceType: "key", ResourceID: "2", ResourceName: "Production Key", Status: "success", CreatedAt: now},
		{UserID: 3, Username: "viewer", Action: "list", ResourceType: "key", ResourceID: "3", ResourceName: "OLD_KEY", Status: "success", CreatedAt: now},
	}
	require.NoError(t, repo.CreateBatch(context.Background(), rows))

	logs, total, err := repo.List(context.Background(), AuditFilter{Q: "test"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "ILike search for 'test' should match 1 row on PG")
	if len(logs) > 0 {
		assert.Equal(t, "TestUser", logs[0].Username)
	}
}

func TestAuditLogRepo_ILike_MySQL(t *testing.T) {
	db, dia, cleanup := setupAuditMySQLTestDB(t)
	defer cleanup()

	repo := NewAuditLogRepo(db, dia)

	now := time.Now()
	rows := []model.AuditLog{
		{UserID: 1, Username: "TestUser", Action: "create", ResourceType: "key", ResourceID: "1", ResourceName: "TestResource", Status: "success", CreatedAt: now},
		{UserID: 2, Username: "admin", Action: "delete", ResourceType: "key", ResourceID: "2", ResourceName: "Production Key", Status: "success", CreatedAt: now},
	}
	require.NoError(t, repo.CreateBatch(context.Background(), rows))

	logs, total, err := repo.List(context.Background(), AuditFilter{Q: "test"})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "ILike search for 'test' should match 1 row on MySQL")
	if len(logs) > 0 {
		assert.Equal(t, "TestUser", logs[0].Username)
	}
}

// setupAuditPGTestDB creates a PG test database with full schema.
func setupAuditPGTestDB(t *testing.T) (*gorm.DB, dialect.Dialect, func()) {
	t.Helper()

	cfg := dialect.DBConfig{
		Driver:   "postgres",
		Host:     envOr("PG_TEST_HOST", "localhost"),
		Port:     envIntOr("PG_TEST_PORT", 5433),
		User:     envOr("PG_TEST_USER", "crosslink"),
		Password: envOr("PG_TEST_PASSWORD", "crosslink_test"),
		DBName:   envOr("PG_TEST_DBNAME", "crosslink_test_pg"),
		SSLMode:  envOr("PG_TEST_SSLMODE", "disable"),
		Timezone: envOr("PG_TEST_TIMEZONE", "UTC"),
	}

	dia := dialect.NewPostgresDialect(cfg)
	db, err := dia.InitDB()
	require.NoError(t, err)

	// Clean slate
	db.Exec("DROP SCHEMA public CASCADE")
	db.Exec("CREATE SCHEMA public")

	// Run migrations
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)
	require.NoError(t, dia.RunMigrations(context.Background()))

	cleanup := func() { dia.Shutdown(db) }
	return db, dia, cleanup
}

// setupAuditMySQLTestDB creates a MySQL test database with full schema.
func setupAuditMySQLTestDB(t *testing.T) (*gorm.DB, dialect.Dialect, func()) {
	t.Helper()

	cfg := dialect.DBConfig{
		Driver:   "mysql",
		Host:     envOr("MYSQL_TEST_HOST", "127.0.0.1"),
		Port:     envIntOr("MYSQL_TEST_PORT", 3307),
		User:     envOr("MYSQL_TEST_USER", "root"),
		Password: envOr("MYSQL_TEST_PASSWORD", "crosslink_test"),
		DBName:   envOr("MYSQL_TEST_DBNAME", "crosslink_test_mysql"),
	}

	dia := dialect.NewMySQLDialect(cfg)
	db, err := dia.InitDB()
	require.NoError(t, err)

	// Clean slate — drop all tables
	dropAllTables(t, db)

	// Run migrations
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)
	require.NoError(t, dia.RunMigrations(context.Background()))

	cleanup := func() { dia.Shutdown(db) }
	return db, dia, cleanup
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &fallback); n == 1 && err == nil {
			return fallback
		}
	}
	return fallback
}

func dropAllTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec("SET FOREIGN_KEY_CHECKS = 0")
	var tables []string
	db.Raw("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables)
	for _, tbl := range tables {
		db.Exec("DROP TABLE IF EXISTS `" + tbl + "`")
	}
	db.Exec("SET FOREIGN_KEY_CHECKS = 1")
}

// splitSQL splits a SQL file into individual statements.
func splitSQLInt(sql string) []string {
	return strings.Split(sql, ";")
}

// truncateStr shortens a string for error messages.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
