package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// KingbaseDialect implements the Dialect interface for KingbaseES (人大金仓).
//
// KingbaseES is PostgreSQL-compatible but returns a non-standard version string
// (e.g. "KingbaseES V008R006C...") which causes jackc/pgx v5 (used by
// gorm.io/driver/postgres v1.5+) to fail during connection handshake.
//
// This dialect bypasses the issue by opening the connection via lib/pq (which
// does not parse the server version string) and passing the established *sql.DB
// to GORM's postgres dialector. All other behavior is identical to PostgresDialect.
type KingbaseDialect struct {
	cfg DBConfig
	pg  *PostgresDialect // delegate for SQL helpers and shared logic
}

// NewKingbaseDialect creates a new KingbaseDialect with the given config.
func NewKingbaseDialect(cfg DBConfig) *KingbaseDialect {
	return &KingbaseDialect{cfg: cfg, pg: NewPostgresDialect(cfg)}
}

// Name returns the dialect identifier.
func (k *KingbaseDialect) Name() string { return "kingbase" }

// MigrationDir returns the path to PostgreSQL migration files (shared with PG).
func (k *KingbaseDialect) MigrationDir() string { return k.pg.MigrationDir() }

// Capabilities returns the same capabilities as PostgreSQL.
func (k *KingbaseDialect) Capabilities() Capabilities { return k.pg.Capabilities() }

// PoolConfig returns the same pool settings as PostgreSQL.
func (k *KingbaseDialect) PoolConfig() PoolConfig { return k.pg.PoolConfig() }

// PartitionSupport returns native partitioning support (same as PG).
func (k *KingbaseDialect) PartitionSupport() PartitionSupport { return k.pg.PartitionSupport() }

// InitDB opens a GORM connection via lib/pq to bypass pgx version string parsing.
//
// Connection flow:
//  1. sql.Open("postgres", dsnURL) — uses lib/pq, no version check
//  2. gorm.Open(postgres.New(postgres.Config{Conn: sqlDB})) — wraps existing connection
func (k *KingbaseDialect) InitDB() (*gorm.DB, error) {
	sqlDB, err := sql.Open("postgres", k.dsnURL())
	if err != nil {
		return nil, fmt.Errorf("open kingbase connection: %w", err)
	}

	pool := k.PoolConfig()
	sqlDB.SetMaxOpenConns(pool.MaxOpenConns)
	sqlDB.SetMaxIdleConns(pool.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(pool.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(pool.ConnMaxIdleTime)

	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open kingbase gorm: %w", err)
	}

	return db, nil
}

// RunMigrations runs golang-migrate using PG migration files.
func (k *KingbaseDialect) RunMigrations(ctx context.Context) error {
	m, err := migrate.New(
		"file://"+k.MigrationDir(),
		k.dsnURL(),
	)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// AcquireMigrationLock takes a pg_advisory_lock on a separate connection.
func (k *KingbaseDialect) AcquireMigrationLock() (func(), error) {
	lockDB, err := sql.Open("postgres", k.dsnURL())
	if err != nil {
		return nil, fmt.Errorf("open kingbase migration lock connection: %w", err)
	}

	if _, err := lockDB.Exec("SELECT pg_advisory_lock(20260518)"); err != nil {
		lockDB.Close()
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}

	release := func() {
		lockDB.Exec("SELECT pg_advisory_unlock(20260518)")
		lockDB.Close()
	}
	return release, nil
}

// EnsureMonthlyPartitions delegates to the PG implementation (same syntax).
func (k *KingbaseDialect) EnsureMonthlyPartitions(ctx context.Context, db *gorm.DB, table string, months int) error {
	return k.pg.EnsureMonthlyPartitions(ctx, db, table, months)
}

// SQL helpers — delegate to PostgresDialect (identical SQL syntax).

func (k *KingbaseDialect) DateTrunc(granularity, column string) string {
	return k.pg.DateTrunc(granularity, column)
}

func (k *KingbaseDialect) DateFormat(column, format string) string {
	return k.pg.DateFormat(column, format)
}

func (k *KingbaseDialect) ILike(column, param string) string {
	return k.pg.ILike(column, param)
}

func (k *KingbaseDialect) JSONMergePatch(column, jsonExpr string) string {
	return k.pg.JSONMergePatch(column, jsonExpr)
}

func (k *KingbaseDialect) ConditionalCount(column, value string) string {
	return k.pg.ConditionalCount(column, value)
}

func (k *KingbaseDialect) CastFloat(expr string) string {
	return k.pg.CastFloat(expr)
}

// Shutdown closes the underlying SQL database connection.
func (k *KingbaseDialect) Shutdown(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// dsnURL returns the postgres:// URL format (compatible with KingbaseES).
func (k *KingbaseDialect) dsnURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		k.cfg.User, k.cfg.Password, k.cfg.Host, k.cfg.Port, k.cfg.DBName, k.cfg.SSLMode)
}

// Suppress unused import warning for strings (used by delegated pg methods).
var _ = strings.Contains
