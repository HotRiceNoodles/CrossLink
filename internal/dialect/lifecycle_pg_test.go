//go:build integration

package dialect

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestPG opens a raw GORM connection to the test PG database.
// Calls t.Skipf if PG is not available.
func openTestPG(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(pgTestDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("PostgreSQL not available: %v", err)
	}
	return db
}

func TestPostgresLifecycle_FullWorkflow(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestPG(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	// Clean slate
	dropAllTablesPG(t, db)

	// Create dialect and run lifecycle
	d := NewPostgresDialect(pgTestDBConfig())

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
		gormDB.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = ?", table).Scan(&count)
		assert.Equal(t, int64(1), count, "table %q should exist", table)
	}

	// Verify capabilities
	cap := d.Capabilities()
	assert.True(t, cap.PartialIndex)
	assert.Equal(t, 100, cap.ConcurrentWrites)
	assert.True(t, cap.AdvisoryLock)
	assert.Equal(t, PartitionNative, d.PartitionSupport())

	// Shutdown
	err = d.Shutdown(gormDB)
	assert.NoError(t, err)

	t.Log("OK: PostgreSQL full lifecycle completed successfully")
}

func TestPostgresLifecycle_MigrationLock(t *testing.T) {
	dsn := pgTestDSN()

	// Open first connection and acquire advisory lock
	lockDB1, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("PostgreSQL not available: %v", err)
	}
	defer lockDB1.Close()

	// Acquire lock on first connection
	_, err = lockDB1.Exec("SELECT pg_advisory_lock(20260518)")
	require.NoError(t, err, "first lock acquisition should succeed")

	// Try to acquire the same lock on a second connection with a timeout
	// pg_advisory_lock is blocking — use a goroutine to test that it blocks
	lockDB2, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer lockDB2.Close()

	acquired := make(chan error, 1)
	go func() {
		// This will block until lock is released
		_, err := lockDB2.Exec("SELECT pg_advisory_lock(20260518)")
		acquired <- err
	}()

	// Wait briefly — the goroutine should still be blocked
	select {
	case <-acquired:
		t.Fatal("second lock should not have been acquired while first is held")
	case <-time.After(500 * time.Millisecond):
		// Expected: second lock is still blocked
	}

	// Release the first lock
	_, err = lockDB1.Exec("SELECT pg_advisory_unlock(20260518)")
	require.NoError(t, err)

	// Now the second lock should be acquired
	select {
	case err := <-acquired:
		assert.NoError(t, err, "second lock should succeed after first is released")
	case <-time.After(5 * time.Second):
		t.Fatal("second lock acquisition timed out after first was released")
	}

	// Clean up: release second lock
	lockDB2.Exec("SELECT pg_advisory_unlock(20260518)")

	t.Log("OK: PostgreSQL migration lock contention works correctly")
}

func TestPostgresLifecycle_IdempotentMigrations(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestPG(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	dropAllTablesPG(t, db)

	d := NewPostgresDialect(pgTestDBConfig())

	gormDB, err := d.InitDB()
	require.NoError(t, err)
	defer d.Shutdown(gormDB)

	// Run migrations twice
	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err, "running migrations twice should not error (ErrNoChange)")

	t.Log("OK: PostgreSQL idempotent migrations work correctly")
}

