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

// openTestKingbase opens a raw GORM connection to the test KingbaseES database.
// Calls t.Skipf if KingbaseES is not available.
func openTestKingbase(t *testing.T) *gorm.DB {
	t.Helper()
	sqlDB, err := sql.Open("postgres", kingbaseTestDSN())
	if err != nil {
		t.Skipf("KingbaseES not available: %v", err)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		sqlDB.Close()
		t.Skipf("KingbaseES not available: %v", err)
	}
	return db
}

func TestKingbaseLifecycle_FullWorkflow(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestKingbase(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	// Clean slate — same as PG since KingbaseES is PG-compatible
	dropAllTablesPG(t, db)

	// Create dialect and run lifecycle
	d := NewKingbaseDialect(kingbaseTestDBConfig())

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

	// Verify capabilities (should match PG)
	cap := d.Capabilities()
	assert.True(t, cap.PartialIndex)
	assert.Equal(t, 100, cap.ConcurrentWrites)
	assert.True(t, cap.AdvisoryLock)
	assert.Equal(t, PartitionNative, d.PartitionSupport())

	// Verify SQL helpers produce PG-compatible output
	assert.Equal(t, `date_trunc('day', "created_at")`, d.DateTrunc("day", "created_at"))
	assert.Contains(t, d.ILike("name", "?"), "ILIKE")

	// Shutdown
	err = d.Shutdown(gormDB)
	assert.NoError(t, err)

	t.Log("OK: KingbaseES full lifecycle completed successfully")
}

func TestKingbaseLifecycle_MigrationLock(t *testing.T) {
	dsn := kingbaseTestDSN()

	// Open first connection and acquire advisory lock
	lockDB1, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("KingbaseES not available: %v", err)
	}
	defer lockDB1.Close()

	// Acquire lock on first connection
	_, err = lockDB1.Exec("SELECT pg_advisory_lock(20260518)")
	require.NoError(t, err, "first lock acquisition should succeed")

	// Try to acquire the same lock on a second connection
	lockDB2, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer lockDB2.Close()

	acquired := make(chan error, 1)
	go func() {
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

	// Clean up
	lockDB2.Exec("SELECT pg_advisory_unlock(20260518)")

	t.Log("OK: KingbaseES migration lock contention works correctly")
}

func TestKingbaseLifecycle_IdempotentMigrations(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestKingbase(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	dropAllTablesPG(t, db)

	d := NewKingbaseDialect(kingbaseTestDBConfig())

	gormDB, err := d.InitDB()
	require.NoError(t, err)
	defer d.Shutdown(gormDB)

	// Run migrations twice
	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err, "running migrations twice should not error (ErrNoChange)")

	t.Log("OK: KingbaseES idempotent migrations work correctly")
}
