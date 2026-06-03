//go:build integration

package dialect

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestMySQL opens a raw GORM connection to the test MySQL database.
// Calls t.Skipf if MySQL is not available.
func openTestMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.Open(mysqlTestDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("MySQL not available: %v", err)
	}
	return db
}

func TestMySQLLifecycle_FullWorkflow(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestMySQL(t)
	dropAllTablesMySQL(t, db)

	d := NewMySQLDialect(mysqlTestDBConfig())

	gormDB, err := d.InitDB()
	require.NoError(t, err)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	// Verify tables exist
	expectedTables := []string{
		"system_settings", "roles", "users", "organizations", "teams",
		"providers", "provider_models", "api_keys", "usage_logs",
		"guardrail_rules", "audit_logs", "insights", "agent_fingerprints",
		"mcp_servers", "mcp_tool_call_logs",
	}
	for _, table := range expectedTables {
		var count int64
		gormDB.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count)
		assert.Equal(t, int64(1), count, "table %q should exist", table)
	}

	// Verify capabilities
	cap := d.Capabilities()
	assert.False(t, cap.PartialIndex)
	assert.Equal(t, 100, cap.ConcurrentWrites)
	assert.True(t, cap.AdvisoryLock)
	assert.Equal(t, PartitionNative, d.PartitionSupport())

	err = d.Shutdown(gormDB)
	assert.NoError(t, err)

	t.Log("OK: MySQL full lifecycle completed successfully")
}

func TestMySQLLifecycle_MigrationLock(t *testing.T) {
	dsn := mysqlTestDSN()

	// First connection: acquire named lock
	lockDB1, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("MySQL not available: %v", err)
	}
	defer lockDB1.Close()

	// Verify connection works
	if err := lockDB1.Ping(); err != nil {
		t.Skipf("MySQL not available: %v", err)
	}

	var result int
	err = lockDB1.QueryRow("SELECT GET_LOCK('crosslink_migration', 5)").Scan(&result)
	require.NoError(t, err)
	require.Equal(t, 1, result, "first GET_LOCK should return 1")

	// Second connection: try same named lock — should block then fail
	lockDB2, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer lockDB2.Close()

	// Use a short timeout (1 second) — should fail because lock is held
	var result2 int
	err = lockDB2.QueryRow("SELECT GET_LOCK('crosslink_migration', 1)").Scan(&result2)
	require.NoError(t, err)
	assert.Equal(t, 0, result2, "second GET_LOCK should return 0 (not acquired)")

	// Release from first connection
	lockDB1.Exec("SELECT RELEASE_LOCK('crosslink_migration')")

	// Now second connection should succeed
	err = lockDB2.QueryRow("SELECT GET_LOCK('crosslink_migration', 5)").Scan(&result2)
	require.NoError(t, err)
	assert.Equal(t, 1, result2, "GET_LOCK should succeed after release")

	// Clean up
	lockDB2.Exec("SELECT RELEASE_LOCK('crosslink_migration')")

	t.Log("OK: MySQL migration lock contention works correctly")
}

func TestMySQLLifecycle_IdempotentMigrations(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestMySQL(t)
	dropAllTablesMySQL(t, db)

	d := NewMySQLDialect(mysqlTestDBConfig())

	gormDB, err := d.InitDB()
	require.NoError(t, err)
	defer d.Shutdown(gormDB)

	// Run migrations twice
	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err, "running migrations twice should not error")

	t.Log("OK: MySQL idempotent migrations work correctly")
}
