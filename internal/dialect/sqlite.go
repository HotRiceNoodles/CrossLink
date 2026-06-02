package dialect

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// SQLiteDialect implements the Dialect interface for SQLite.
type SQLiteDialect struct {
	cfg DBConfig
	db  *gorm.DB // stored after InitDB for RunMigrations to use
}

// NewSQLiteDialect creates a new SQLiteDialect with the given config.
func NewSQLiteDialect(cfg DBConfig) *SQLiteDialect {
	return &SQLiteDialect{cfg: cfg}
}

// Name returns the dialect identifier.
func (s *SQLiteDialect) Name() string { return "sqlite" }

// MigrationDir returns the path to SQLite migration files.
func (s *SQLiteDialect) MigrationDir() string { return "migrations/sqlite" }

// PartitionSupport returns no partitioning support for SQLite.
func (s *SQLiteDialect) PartitionSupport() PartitionSupport { return PartitionNone }

// Capabilities returns SQLite's feature support.
func (s *SQLiteDialect) Capabilities() Capabilities {
	return Capabilities{
		PartialIndex:     true, // SQLite 3.9.0+
		ConcurrentWrites: 1,    // Single writer
		AdvisoryLock:     false, // Uses file lock instead
	}
}

// PoolConfig returns the recommended connection pool settings for SQLite.
func (s *SQLiteDialect) PoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: 0, // No recycling
		ConnMaxIdleTime: 0,
	}
}

// InitDB creates the SQLite connection with WAL mode, foreign keys, and parent directory.
func (s *SQLiteDialect) InitDB() (*gorm.DB, error) {
	// 1. Ensure parent directory exists
	dir := filepath.Dir(s.cfg.SQLitePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}

	// 2. glebarez/sqlite pure Go driver with PRAGMA via DSN
	// busy_timeout is hardcoded to 5000ms by the driver itself
	dsn := s.cfg.SQLitePath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// 3. Single connection pool
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	sqlDB.SetConnMaxIdleTime(0)

	s.db = db
	slog.Info("database connected", "driver", s.Name(), "path", s.cfg.SQLitePath)
	return db, nil
}

// RunMigrations reads the SQLite migration file, splits it into individual statements,
// executes each in a transaction, and records the version in schema_migrations.
func (s *SQLiteDialect) RunMigrations(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("InitDB must be called before RunMigrations")
	}

	migrationFile := filepath.Join(s.MigrationDir(), "000001_init_schema.up.sql")
	sqlBytes, err := os.ReadFile(migrationFile)
	if err != nil {
		return fmt.Errorf("read sqlite migration: %w", err)
	}

	// Split into individual statements and execute in a transaction
	statements := splitSQL(string(sqlBytes))
	tx := s.db.Begin()
	for i, stmt := range statements {
		if stmt == "" {
			continue
		}
		if err := tx.Exec(stmt).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("statement %d: %w\n%s", i, err, truncate(stmt, 200))
		}
	}
	tx.Commit()

	// Record version in schema_migrations
	s.db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, dirty BOOLEAN)")
	s.db.Exec("INSERT OR IGNORE INTO schema_migrations VALUES ('1', false)")

	slog.Info("database migrated successfully", "driver", s.Name())
	return nil
}

// AcquireMigrationLock uses a file lock for SQLite.
func (s *SQLiteDialect) AcquireMigrationLock() (func(), error) {
	lockPath := s.cfg.SQLitePath + ".migration.lock"
	fileLock := flock.New(lockPath)
	ok, err := fileLock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("sqlite migration lock: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("sqlite migration lock contention")
	}
	return func() { fileLock.Unlock() }, nil
}

// EnsureMonthlyPartitions archives old mcp_tool_call_logs records (older than 90 days)
// into mcp_tool_call_logs_archive in batches. For SQLite, this replaces PG's native
// partitioning with a manual archive-and-delete approach.
func (s *SQLiteDialect) EnsureMonthlyPartitions(ctx context.Context, db *gorm.DB, table string, months int) error {
	if table != "mcp_tool_call_logs" {
		return nil // only archive mcp_tool_call_logs
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -90)

	const batchSize = 500
	for {
		// Move old records to archive in batches
		result := db.WithContext(ctx).Exec(
			"INSERT OR IGNORE INTO mcp_tool_call_logs_archive "+
				"(id, request_id, server_id, server_name, tool_name, method, input_size, output_size, "+
				"duration, status, error_code, error_msg, api_key_id, user_id, team_id, blocked_by, created_at) "+
				"SELECT id, request_id, server_id, server_name, tool_name, method, input_size, output_size, "+
				"duration, status, error_code, error_msg, api_key_id, user_id, team_id, blocked_by, created_at "+
				"FROM mcp_tool_call_logs WHERE created_at < ? AND id IN "+
				"(SELECT id FROM mcp_tool_call_logs WHERE created_at < ? LIMIT ?)",
			cutoff, cutoff, batchSize,
		)
		if result.Error != nil {
			return fmt.Errorf("archive mcp_tool_call_logs: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			break
		}

		// Delete archived records
		db.WithContext(ctx).Exec(
			"DELETE FROM mcp_tool_call_logs WHERE created_at < ? AND id IN "+
				"(SELECT id FROM mcp_tool_call_logs WHERE created_at < ? LIMIT ?)",
			cutoff, cutoff, batchSize,
		)

		time.Sleep(50 * time.Millisecond)
	}

	return nil
}

// DateTrunc returns a date truncation expression for SQLite.
func (s *SQLiteDialect) DateTrunc(granularity string, column string) string {
	switch granularity {
	case "hour":
		return fmt.Sprintf("strftime('%%Y-%%m-%%d %%H:00:00', %s)", column)
	default: // "day" and others
		return fmt.Sprintf("DATE(%s)", column)
	}
}

// DateFormat returns a strftime expression for SQLite.
func (s *SQLiteDialect) DateFormat(column string, format string) string {
	return fmt.Sprintf("strftime('%s', %s)", format, column)
}

// ILike returns a LIKE expression for SQLite (case-insensitive by default for ASCII).
func (s *SQLiteDialect) ILike(column string, param string) string {
	return fmt.Sprintf("%s LIKE %s", column, param)
}

// JSONMergePatch returns a json_patch expression for SQLite.
func (s *SQLiteDialect) JSONMergePatch(column string, jsonExpr string) string {
	return fmt.Sprintf("json_patch(COALESCE(%s, '{}'), %s)", column, jsonExpr)
}

// Shutdown performs WAL checkpoint and closes the connection.
func (s *SQLiteDialect) Shutdown(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	sqlDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return sqlDB.Close()
}

// splitSQL splits SQL text into individual statements, ignoring comments and empty lines.
func splitSQL(sql string) []string {
	var stmts []string
	var current strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			s := strings.TrimSpace(current.String())
			if s != "" {
				stmts = append(stmts, s)
			}
			current.Reset()
		}
	}
	if current.Len() > 0 {
		s := strings.TrimSpace(current.String())
		if s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

// truncate truncates a string to at most n characters.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
