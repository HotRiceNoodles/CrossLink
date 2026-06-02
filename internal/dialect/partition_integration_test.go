//go:build integration

package dialect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestPostgres_EnsureMonthlyPartitions(t *testing.T) {
	db, _, cleanup := setupPGTestDB(t)
	defer cleanup()

	d := NewPostgresDialect(pgTestDBConfig())
	ctx := context.Background()

	// The mcp_tool_call_logs table should be partitioned after migrations
	err := d.EnsureMonthlyPartitions(ctx, db, "mcp_tool_call_logs", 3)
	require.NoError(t, err)

	// Verify partitions were created in pg_catalog
	now := time.Now()
	for i := 0; i < 3; i++ {
		tm := now.AddDate(0, i, 0)
		partName := fmt.Sprintf("mcp_tool_call_logs_%d_%02d", tm.Year(), tm.Month())
		var count int64
		db.Raw(
			"SELECT count(*) FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class c ON i.inhrelid = c.oid WHERE c.relname = ?",
			partName,
		).Scan(&count)
		assert.Equal(t, int64(1), count, "partition %q should exist", partName)
	}

	// Verify default partition exists
	var defaultCount int64
	db.Raw(
		"SELECT count(*) FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class c ON i.inhrelid = c.oid WHERE c.relname = 'mcp_tool_call_logs_default'",
	).Scan(&defaultCount)
	assert.Equal(t, int64(1), defaultCount, "default partition should exist")

	t.Log("OK: PostgreSQL EnsureMonthlyPartitions created correct partitions")
}

func TestPostgres_PartitionRouting(t *testing.T) {
	db, _, cleanup := setupPGTestDB(t)
	defer cleanup()

	d := NewPostgresDialect(pgTestDBConfig())
	ctx := context.Background()
	require.NoError(t, d.EnsureMonthlyPartitions(ctx, db, "mcp_tool_call_logs", 3))

	// Insert a row in the current month
	now := time.Now()
	err := db.Exec(
		"INSERT INTO mcp_tool_call_logs (request_id, server_id, server_name, tool_name, status, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		"req-partition-test", 1, "test-server", "test-tool", "success", now,
	).Error
	require.NoError(t, err)

	// Verify it's queryable from the parent table
	var count int64
	db.Raw("SELECT count(*) FROM mcp_tool_call_logs WHERE request_id = ?", "req-partition-test").Scan(&count)
	assert.Equal(t, int64(1), count, "row should be queryable from parent table")

	db.Exec("DELETE FROM mcp_tool_call_logs WHERE request_id = ?", "req-partition-test")

	t.Log("OK: PostgreSQL partition routing works correctly")
}

func TestMySQL_EnsureMonthlyPartitions(t *testing.T) {
	db, _, cleanup := setupMySQLTestDB(t)
	defer cleanup()

	d := NewMySQLDialect(mysqlTestDBConfig())
	ctx := context.Background()

	err := d.EnsureMonthlyPartitions(ctx, db, "mcp_tool_call_logs", 3)
	require.NoError(t, err)

	// Verify new partitions were created
	now := time.Now()
	for i := 0; i < 3; i++ {
		tm := now.AddDate(0, i, 0)
		partName := fmt.Sprintf("p_%d_%02d", tm.Year(), tm.Month())
		var count int64
		db.Raw(
			"SELECT count(*) FROM information_schema.partitions WHERE table_schema = DATABASE() AND table_name = 'mcp_tool_call_logs' AND partition_name = ?",
			partName,
		).Scan(&count)
		assert.GreaterOrEqual(t, count, int64(1), "partition %q should exist", partName)
	}

	t.Log("OK: MySQL EnsureMonthlyPartitions created correct partitions")
}

func TestSQLite_EnsureMonthlyPartitions(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	// Create in-memory SQLite with full schema
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	db.Exec("PRAGMA foreign_keys = ON")

	schemaBytes, err := os.ReadFile(filepath.Join("migrations", "sqlite", "000001_init_schema.up.sql"))
	require.NoError(t, err)
	for _, stmt := range splitSQL(string(schemaBytes)) {
		if stmt != "" {
			require.NoError(t, db.Exec(stmt).Error)
		}
	}

	d := &SQLiteDialect{}
	ctx := context.Background()

	// Insert old records (>90 days) into mcp_tool_call_logs
	oldDate := time.Now().AddDate(0, 0, -120)
	for i := 0; i < 5; i++ {
		db.Exec(
			"INSERT INTO mcp_tool_call_logs (request_id, server_id, server_name, tool_name, status, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			fmt.Sprintf("req-old-%d", i), 1, "test-server", "test-tool", "success",
			oldDate.Format("2006-01-02 15:04:05"),
		)
	}

	// Insert a recent record (should NOT be archived)
	recentDate := time.Now().AddDate(0, 0, -10)
	db.Exec(
		"INSERT INTO mcp_tool_call_logs (request_id, server_id, server_name, tool_name, status, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		"req-recent", 1, "test-server", "test-tool", "success",
		recentDate.Format("2006-01-02 15:04:05"),
	)

	// Run partition archival
	err = d.EnsureMonthlyPartitions(ctx, db, "mcp_tool_call_logs", 3)
	require.NoError(t, err)

	// Verify old records moved to archive
	var archiveCount int64
	db.Raw("SELECT count(*) FROM mcp_tool_call_logs_archive").Scan(&archiveCount)
	assert.Equal(t, int64(5), archiveCount, "old records should be in archive table")

	// Verify main table only has recent record
	var mainCount int64
	db.Raw("SELECT count(*) FROM mcp_tool_call_logs").Scan(&mainCount)
	assert.Equal(t, int64(1), mainCount, "only recent record should remain in main table")

	// Verify the recent record is the right one
	var recentReq string
	db.Raw("SELECT request_id FROM mcp_tool_call_logs LIMIT 1").Scan(&recentReq)
	assert.Equal(t, "req-recent", recentReq)

	t.Log("OK: SQLite EnsureMonthlyPartitions archived old records correctly")
}
