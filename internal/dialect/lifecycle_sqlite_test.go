package dialect

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteLifecycle_FullWorkflow(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	dir, err := os.MkdirTemp("", "crosslink-sqlite-lifecycle-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "test.db")
	d := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: dbPath})

	// Step 1: InitDB
	db, err := d.InitDB()
	require.NoError(t, err)
	require.NotNil(t, db)

	// Step 2: RunMigrations
	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	// Step 3: Verify schema_migrations
	var version string
	db.Raw("SELECT version FROM schema_migrations LIMIT 1").Scan(&version)
	assert.Equal(t, "1", version)

	// Step 4: Verify all expected tables exist
	expectedTables := []string{
		"system_settings", "roles", "users", "organizations", "organization_members",
		"teams", "team_members", "role_permissions", "providers", "provider_models",
		"api_keys", "api_key_hashes", "usage_logs", "guardrail_rules",
		"guardrail_alert_rules", "guardrail_alert_logs", "budget_alerts",
		"budget_snapshots", "budget_recommendations", "budget_requests",
		"audit_logs", "insights", "optimization_actions", "agent_fingerprints",
		"mcp_servers", "mcp_server_permissions", "mcp_tool_call_logs",
		"mcp_tool_call_logs_archive",
	}
	for _, table := range expectedTables {
		var count int64
		db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
		assert.Equal(t, int64(1), count, "table %q should exist", table)
	}

	// Step 5: Capabilities
	cap := d.Capabilities()
	assert.True(t, cap.PartialIndex)
	assert.Equal(t, 1, cap.ConcurrentWrites)
	assert.False(t, cap.AdvisoryLock)

	// Step 6: PartitionSupport
	assert.Equal(t, PartitionNone, d.PartitionSupport())

	// Step 7: Shutdown
	err = d.Shutdown(db)
	assert.NoError(t, err)

	// Step 8: Verify database file persists after WAL checkpoint
	_, err = os.Stat(dbPath)
	assert.NoError(t, err, "database file should exist after shutdown")

	t.Log("OK: SQLite full lifecycle completed successfully")
}

func TestSQLiteLifecycle_MigrationLock(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	dir, err := os.MkdirTemp("", "crosslink-sqlite-lock-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "test.db")
	d := NewSQLiteDialect(DBConfig{Driver: "sqlite", SQLitePath: dbPath})

	// First lock should succeed
	release, err := d.AcquireMigrationLock()
	require.NoError(t, err)
	require.NotNil(t, release)

	// Second lock attempt on the same path should fail (file lock contention)
	_, err = d.AcquireMigrationLock()
	assert.Error(t, err, "second AcquireMigrationLock should fail due to contention")

	// Release the first lock
	release()

	// Now a new lock should succeed
	release2, err := d.AcquireMigrationLock()
	assert.NoError(t, err, "AcquireMigrationLock should succeed after release")
	if release2 != nil {
		release2()
	}

	t.Log("OK: SQLite migration lock contention works correctly")
}
