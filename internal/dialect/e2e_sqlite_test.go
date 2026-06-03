package dialect

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSQLiteE2E_FullServerPath validates the complete dialect+migration lifecycle:
// InitDB → RunMigrations → verify all tables → insert → Shutdown → re-open → verify data → Shutdown.
func TestSQLiteE2E_FullServerPath(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	dir, err := os.MkdirTemp("", "crosslink-e2e-sqlite-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "e2e.db")
	cfg := DBConfig{Driver: "sqlite", SQLitePath: dbPath}
	d := NewSQLiteDialect(cfg)

	// ---- Phase 1: Fresh start ----
	db, err := d.InitDB()
	require.NoError(t, err)
	require.NotNil(t, db)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	// Verify all expected tables exist
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

	// Verify schema_migrations version
	var version string
	db.Raw("SELECT version FROM schema_migrations LIMIT 1").Scan(&version)
	assert.Equal(t, "1", version)

	// Verify capabilities
	cap := d.Capabilities()
	assert.True(t, cap.PartialIndex)
	assert.Equal(t, 1, cap.ConcurrentWrites)
	assert.False(t, cap.AdvisoryLock)
	assert.Equal(t, PartitionNone, d.PartitionSupport())

	// Insert rows into core tables to verify schema compatibility
	result := db.Exec(
		"INSERT INTO system_settings (key, value, updated_at) VALUES (?, ?, datetime('now'))",
		"e2e_key", "e2e_value",
	)
	assert.NoError(t, result.Error)
	assert.Equal(t, int64(1), result.RowsAffected)

	// Insert into a table with JSON column
	result = db.Exec(
		"INSERT INTO providers (name, display_name, adapter_type, base_url, api_key, extra_config, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"e2e-provider", "E2E Provider", "openai_compatible", "https://api.example.com", "sk-test", `{"region":"us"}`, 1,
	)
	assert.NoError(t, result.Error)

	// Shutdown
	err = d.Shutdown(db)
	assert.NoError(t, err)

	// ---- Phase 2: Re-open and verify data persistence ----
	db2, err := d.InitDB()
	require.NoError(t, err)
	require.NotNil(t, db2)

	// Verify data survived the shutdown/re-open cycle
	var val string
	db2.Raw("SELECT value FROM system_settings WHERE key = ?", "e2e_key").Scan(&val)
	assert.Equal(t, "e2e_value", val, "data should survive re-open")

	var providerName string
	db2.Raw("SELECT name FROM providers WHERE name = ?", "e2e-provider").Scan(&providerName)
	assert.Equal(t, "e2e-provider", providerName)

	err = d.Shutdown(db2)
	assert.NoError(t, err)

	// Verify database file persists
	_, err = os.Stat(dbPath)
	assert.NoError(t, err, "database file should exist after shutdown")

	t.Log("OK: SQLite E2E full server path completed successfully")
}
