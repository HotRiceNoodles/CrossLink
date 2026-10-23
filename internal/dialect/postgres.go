package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// PostgresDialect implements the Dialect interface for PostgreSQL.
type PostgresDialect struct {
	cfg DBConfig
}

// NewPostgresDialect creates a new PostgresDialect with the given config.
func NewPostgresDialect(cfg DBConfig) *PostgresDialect {
	return &PostgresDialect{cfg: cfg}
}

// Name returns the dialect identifier.
func (p *PostgresDialect) Name() string {
	return "postgres"
}

// MigrationDir returns the path to PostgreSQL migration files.
func (p *PostgresDialect) MigrationDir() string {
	return "migrations/postgres"
}

// Capabilities returns PostgreSQL's feature support.
func (p *PostgresDialect) Capabilities() Capabilities {
	return Capabilities{
		PartialIndex:     true,
		ConcurrentWrites: 100,
		AdvisoryLock:     true,
	}
}

// PoolConfig returns the recommended connection pool settings for PostgreSQL.
func (p *PostgresDialect) PoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    100,
		MaxIdleConns:    50,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 1 * time.Minute,
	}
}

// PartitionSupport returns native partitioning support.
func (p *PostgresDialect) PartitionSupport() PartitionSupport {
	return PartitionNative
}

// InitDB opens a GORM connection to PostgreSQL with pool settings.
func (p *PostgresDialect) InitDB() (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(p.dsn()), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}

	pool := applyPoolOverrides(p.PoolConfig(), p.cfg)
	sqlDB.SetMaxOpenConns(pool.MaxOpenConns)
	sqlDB.SetMaxIdleConns(pool.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(pool.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
	// F-3: the effective pool config is part of the capacity evidence — log
	// it at boot so every run's ground truth is in its own gw.log (ladder
	// campaigns observed behavior consistent with the DIALECT defaults on
	// some launches and the env overrides on others; this line makes the
	// divergence diagnosable per-process instead of inferential).
	slog.Info("postgres pool armed",
		"max_open", pool.MaxOpenConns, "max_idle", pool.MaxIdleConns,
		"conn_max_lifetime", pool.ConnMaxLifetime.String(), "conn_max_idle_time", pool.ConnMaxIdleTime.String())

	return db, nil
}

// RunMigrations runs golang-migrate using files from MigrationDir.
func (p *PostgresDialect) RunMigrations(ctx context.Context) error {
	m, err := migrate.New(
		"file://"+p.MigrationDir(),
		p.dsnURL(),
	)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// AcquireMigrationLock takes a pg_advisory_lock on a separate connection
// to prevent concurrent migration execution in multi-instance deployments.
// The returned function releases the lock and closes the connection.
func (p *PostgresDialect) AcquireMigrationLock() (func(), error) {
	lockDB, err := sql.Open("postgres", p.dsnURL())
	if err != nil {
		return nil, fmt.Errorf("open migration lock connection: %w", err)
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

// EnsureMonthlyPartitions creates monthly partitions for the given table,
// covering from the current month through months-1 months ahead, plus a default partition.
func (p *PostgresDialect) EnsureMonthlyPartitions(ctx context.Context, db *gorm.DB, table string, months int) error {
	now := time.Now()
	for i := 0; i < months; i++ {
		t := now.AddDate(0, i, 0)
		next := t.AddDate(0, 1, 0)
		partName := fmt.Sprintf("%s_%d_%02d", table, t.Year(), t.Month())
		sql := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
			partName, table, t.Format("2006-01-02"), next.Format("2006-01-02"),
		)
		if err := db.WithContext(ctx).Exec(sql).Error; err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create partition %s: %w", partName, err)
			}
		}
	}
	// Ensure default partition exists
	defPart := fmt.Sprintf("%s_default", table)
	sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s DEFAULT", defPart, table)
	if err := db.WithContext(ctx).Exec(sql).Error; err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("create default partition: %w", err)
		}
	}
	return nil
}

// DateTrunc returns a date_trunc expression for PostgreSQL.
func (p *PostgresDialect) DateTrunc(granularity string, column string) string {
	return fmt.Sprintf("date_trunc('%s', %s)", granularity, column)
}

// DateFormat returns a TO_CHAR expression for PostgreSQL, converting %Y/%m/%d to YYYY/MM/DD.
func (p *PostgresDialect) DateFormat(column string, format string) string {
	pgFormat := strings.ReplaceAll(format, "%Y", "YYYY")
	pgFormat = strings.ReplaceAll(pgFormat, "%m", "MM")
	pgFormat = strings.ReplaceAll(pgFormat, "%d", "DD")
	return fmt.Sprintf("TO_CHAR(%s, '%s')", column, pgFormat)
}

// ILike returns a case-insensitive LIKE expression for PostgreSQL.
func (p *PostgresDialect) ILike(column string, param string) string {
	return fmt.Sprintf("%s ILIKE %s", column, param)
}

// JSONMergePatch returns a JSON merge/patch expression for PostgreSQL using jsonb concatenation.
func (p *PostgresDialect) JSONMergePatch(column string, jsonExpr string) string {
	return fmt.Sprintf("COALESCE(%s::jsonb, '{}') || %s::jsonb", column, jsonExpr)
}

// ConditionalCount returns a FILTER (WHERE ...) expression for conditional counting.
func (p *PostgresDialect) ConditionalCount(column string, value string) string {
	return fmt.Sprintf("COUNT(*) FILTER (WHERE %s = %s)", column, value)
}

// ConditionalSum returns a SUM(1) FILTER (WHERE ...) expression for conditional summing.
func (p *PostgresDialect) ConditionalSum(condition string) string {
	return fmt.Sprintf("COALESCE(SUM(1) FILTER (WHERE %s), 0)", condition)
}

// ConditionalSumCol returns a SUM(COALESCE(column,0)) FILTER (WHERE ...) expression.
func (p *PostgresDialect) ConditionalSumCol(column string, condition string) string {
	return fmt.Sprintf("COALESCE(SUM(COALESCE(%s, 0)) FILTER (WHERE %s), 0)", column, condition)
}

// ConditionalCountWhere returns a COUNT(*) FILTER (WHERE ...) expression.
func (p *PostgresDialect) ConditionalCountWhere(condition string) string {
	return fmt.Sprintf("COUNT(*) FILTER (WHERE %s)", condition)
}

// CastFloat returns a PostgreSQL float cast expression.
func (p *PostgresDialect) CastFloat(expr string) string {
	return expr + "::float"
}

// Shutdown closes the underlying SQL database connection.
func (p *PostgresDialect) Shutdown(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// dsn returns the PostgreSQL keyword=value connection string.
// pgApplicationName tags every gateway connection in pg_stat_activity and
// the connection logs (F-3: campaign forensics could not distinguish gateway
// conns from psql probes — application_name makes the source explicit per
// connection). Overridable for multi-instance deployments via CL_DB_APP_NAME.
var pgApplicationName = func() string {
	if v := os.Getenv("CL_DB_APP_NAME"); v != "" {
		return v
	}
	return "crosslink_gateway"
}()

// pgConnectTimeout bounds EVERY postgres dial (boot open, pool refill,
// migration, advisory lock). F-bb (r78/r78e): without it lib/pq waits
// indefinitely for the startup message of a silent accept-hold listener, so
// a blackhole DB at BOOT hung the gateway before it could report anything.
// With it the boot dial fails in bounded time and the process exits with a
// clear "open database" error (fail-fast restart loop) instead of hanging.
// Default 5s; override via CL_DB_CONNECT_TIMEOUT (seconds, integers only).
var pgConnectTimeout = func() string {
	if v := os.Getenv("CL_DB_CONNECT_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 300 {
			return v
		}
	}
	return "5"
}()

func (p *PostgresDialect) dsn() string {
	s := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s application_name=%s connect_timeout=%s",
		p.cfg.Host, p.cfg.Port, p.cfg.User, p.cfg.Password, p.cfg.DBName, p.cfg.SSLMode, pgApplicationName, pgConnectTimeout)
	if p.cfg.Timezone != "" {
		s += fmt.Sprintf(" timezone=%s", p.cfg.Timezone)
	}
	return s
}

// dsnURL returns the postgres:// URL format for golang-migrate.
func (p *PostgresDialect) dsnURL() string {
	s := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s&application_name=%s&connect_timeout=%s",
		p.cfg.User, p.cfg.Password, p.cfg.Host, p.cfg.Port, p.cfg.DBName, p.cfg.SSLMode, pgApplicationName+"_migrate", pgConnectTimeout)
	if p.cfg.Timezone != "" {
		s += fmt.Sprintf("&timezone=%s", p.cfg.Timezone)
	}
	return s
}
