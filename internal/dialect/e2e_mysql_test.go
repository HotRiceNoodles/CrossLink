//go:build integration

package dialect

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMySQLE2E_FullServerPath validates the complete MySQL dialect+migration lifecycle:
// InitDB → clean slate → RunMigrations → verify tables → insert → Shutdown → re-open → re-migrate (idempotent).
func TestMySQLE2E_FullServerPath(t *testing.T) {
	cfg := mysqlTestDBConfig()
	d := NewMySQLDialect(cfg)

	// ---- Phase 1: Fresh start ----
	db, err := d.InitDB()
	require.NoError(t, err)
	require.NotNil(t, db)

	// Clean slate
	dropAllTablesMySQL(t, db)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	// Verify expected tables exist
	expectedTables := []string{
		"system_settings", "roles", "users", "organizations", "organization_members",
		"teams", "team_members", "role_permissions", "providers", "provider_models",
		"api_keys", "api_key_hashes", "usage_logs", "guardrail_rules",
		"guardrail_alert_rules", "guardrail_alert_logs", "budget_alerts",
		"budget_snapshots", "budget_recommendations", "budget_requests",
		"audit_logs", "insights", "optimization_actions", "agent_fingerprints",
		"mcp_servers", "mcp_server_permissions", "mcp_tool_call_logs",
		"mcp_tool_call_logs_archive", "schema_migrations",
	}
	for _, table := range expectedTables {
		var count int64
		db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count)
		assert.Equal(t, int64(1), count, "table %q should exist", table)
	}

	// Verify capabilities
	cap := d.Capabilities()
	assert.True(t, cap.PartialIndex)
	assert.True(t, cap.AdvisoryLock)
	assert.True(t, cap.ConcurrentWrites > 1)

	// Insert one row into a core table
	result := db.Exec(
		"INSERT INTO system_settings (`key`, value, updated_at) VALUES (?, ?, NOW(3))",
		"e2e_test_key", "e2e_test_value",
	)
	assert.NoError(t, result.Error)
	assert.Equal(t, int64(1), result.RowsAffected)

	var val string
	db.Raw("SELECT value FROM system_settings WHERE `key` = ?", "e2e_test_key").Scan(&val)
	assert.Equal(t, "e2e_test_value", val)

	// ---- Phase 2: Idempotent re-migration ----
	err = d.RunMigrations(context.Background())
	assert.NoError(t, err, "re-running migrations should be idempotent")

	// Verify data survived
	var val2 string
	db.Raw("SELECT value FROM system_settings WHERE `key` = ?", "e2e_test_key").Scan(&val2)
	assert.Equal(t, "e2e_test_value", val2)

	// Shutdown
	err = d.Shutdown(db)
	assert.NoError(t, err)

	t.Log("OK: MySQL E2E full server path completed successfully")
}
