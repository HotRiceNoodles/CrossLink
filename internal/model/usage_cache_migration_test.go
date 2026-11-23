package model

import (
	"os"
	"path/filepath"
	"testing"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Cache-fidelity acceptance: the 000111 migration must apply cleanly on the
// pre-migration table shape and round-trip down. Validating the SQL itself
// matters: three dialect files, hand-written. In-memory DB (the migration is
// pure DDL; a file DB trips Windows TempDir cleanup locks).
func TestUsageCacheCreationMigrationSQLite(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)

	// Pre-migration shape: the table WITHOUT cache_creation_tokens.
	require.NoError(t, db.Exec(`CREATE TABLE usage_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL DEFAULT '',
		cache_read_tokens INTEGER NOT NULL DEFAULT 0
	)`).Error)

	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "sqlite", "000111_usage_cache_creation.up.sql"))
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(up)).Error, "up migration must apply on the pre-migration shape")

	// The new column exists and defaults to 0.
	var zero int64
	require.NoError(t, db.Raw(`INSERT INTO usage_logs (request_id) VALUES ('r1')`).Error)
	require.NoError(t, db.Raw(`SELECT cache_creation_tokens FROM usage_logs WHERE request_id='r1'`).Scan(&zero).Error)
	require.Zero(t, zero)

	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "sqlite", "000111_usage_cache_creation.down.sql"))
	require.NoError(t, err)
	require.NoError(t, db.Exec(string(down)).Error, "down migration must apply cleanly")
}
