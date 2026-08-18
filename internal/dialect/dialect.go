package dialect

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Dialect isolates database-specific behavior behind a unified interface.
// Each supported database (postgres, mysql, sqlite) implements this interface
// to handle connection management, migrations, locking, and partitioning.
type Dialect interface {
	// Identity
	Name() string // "postgres", "mysql", "sqlite"

	// Migration
	MigrationDir() string                             // e.g. "migrations/postgres"
	RunMigrations(ctx context.Context) error           // PG/MySQL: golang-migrate; SQLite: direct SQL
	AcquireMigrationLock() (func(), error)             // returns release function

	// Partitioning
	PartitionSupport() PartitionSupport
	EnsureMonthlyPartitions(ctx context.Context, db *gorm.DB, table string, months int) error

	// SQL helpers for cross-database query construction.
	DateTrunc(granularity string, column string) string  // "day"|"hour" → SQL expression
	DateFormat(column string, format string) string      // "%Y-%m" style → SQL expression
	ILike(column string, param string) string            // case-insensitive LIKE
	JSONMergePatch(column string, jsonExpr string) string // JSON merge/patch
	ConditionalCount(column string, value string) string // PG: FILTER(WHERE), others: SUM(CASE)
	ConditionalSum(condition string) string              // PG: SUM(1) FILTER (WHERE cond), others: SUM(CASE WHEN cond THEN 1 ELSE 0 END)
	ConditionalSumCol(column string, condition string) string // SUM(COALESCE(col,0)) under cond — PG: FILTER (WHERE cond), others: SUM(CASE WHEN cond THEN COALESCE(col,0) ELSE 0 END)
	ConditionalCountWhere(condition string) string       // COUNT of rows matching cond — PG: COUNT(*) FILTER (WHERE cond), others: SUM(CASE WHEN cond THEN 1 ELSE 0 END)
	CastFloat(expr string) string                       // PG: ::float, others: CAST(AS DOUBLE)

	// Lifecycle
	Capabilities() Capabilities
	PoolConfig() PoolConfig
	InitDB() (*gorm.DB, error)  // creates connection (with dir creation, PRAGMA, pool config)
	Shutdown(db *gorm.DB) error // cleanup (SQLite: WAL checkpoint + Close)
}

// Capabilities describes what the database engine supports.
type Capabilities struct {
	PartialIndex     bool // WHERE clause on index (PG: yes, MySQL: no, SQLite: yes)
	ConcurrentWrites int  // Max concurrent writers (SQLite = 1)
	AdvisoryLock     bool // Application-level lock (PG/MySQL: yes, SQLite: file lock)
}

// PoolConfig holds connection pool settings.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// PartitionSupport indicates the database's partitioning capability.
type PartitionSupport int

const (
	PartitionNone    PartitionSupport = iota // SQLite — plain tables + archival
	PartitionNative                          // PostgreSQL, MySQL — native RANGE partitioning
)

// DBConfig is the database-agnostic config passed to dialect implementations.
// It mirrors config.DatabaseConfig but adds Driver and SQLitePath fields
// so the dialect layer doesn't depend on the config package.
type DBConfig struct {
	Driver     string
	Host       string
	Port       int
	User       string
	Password   string
	DBName     string
	SSLMode    string
	Timezone   string // e.g. "UTC" — appended to PG/MySQL DSN
	SQLitePath string
}
