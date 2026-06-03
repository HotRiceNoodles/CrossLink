package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	mysqlDriver "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// MySQLDialect implements the Dialect interface for MySQL.
type MySQLDialect struct {
	cfg DBConfig
}

// NewMySQLDialect creates a new MySQLDialect with the given config.
func NewMySQLDialect(cfg DBConfig) *MySQLDialect {
	return &MySQLDialect{cfg: cfg}
}

// Name returns the dialect identifier.
func (m *MySQLDialect) Name() string { return "mysql" }

// MigrationDir returns the path to MySQL migration files.
func (m *MySQLDialect) MigrationDir() string { return "migrations/mysql" }

// Capabilities returns MySQL's feature support.
func (m *MySQLDialect) Capabilities() Capabilities {
	return Capabilities{
		PartialIndex:     false, // MySQL doesn't support partial indexes
		ConcurrentWrites: 100,
		AdvisoryLock:     true, // GET_LOCK/RELEASE_LOCK
	}
}

// PoolConfig returns the recommended connection pool settings for MySQL.
func (m *MySQLDialect) PoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    100,
		MaxIdleConns:    50,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: time.Minute,
	}
}

// PartitionSupport returns native partitioning support for MySQL.
func (m *MySQLDialect) PartitionSupport() PartitionSupport { return PartitionNative }

// InitDB opens a GORM connection to MySQL with pool settings.
func (m *MySQLDialect) InitDB() (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(m.dsn()), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}
	pc := m.PoolConfig()
	sqlDB.SetMaxOpenConns(pc.MaxOpenConns)
	sqlDB.SetMaxIdleConns(pc.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(pc.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(pc.ConnMaxIdleTime)
	slog.Info("database connected", "driver", m.Name(), "host", m.cfg.Host, "dbname", m.cfg.DBName)
	return db, nil
}

// RunMigrations runs golang-migrate using files from MigrationDir.
func (m *MySQLDialect) RunMigrations(ctx context.Context) error {
	db, err := sql.Open("mysql", m.dsn())
	if err != nil {
		return fmt.Errorf("open mysql for migration: %w", err)
	}
	defer db.Close()

	driver, err := mysqlDriver.WithInstance(db, &mysqlDriver.Config{})
	if err != nil {
		return fmt.Errorf("create mysql driver: %w", err)
	}

	mg, err := migrate.NewWithDatabaseInstance(
		"file://"+m.MigrationDir(),
		"mysql",
		driver,
	)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	if err := mg.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}
	slog.Info("database migrated successfully", "driver", m.Name())
	return nil
}

// AcquireMigrationLock uses MySQL's GET_LOCK to prevent concurrent migrations.
// The returned function releases the lock and closes the connection.
func (m *MySQLDialect) AcquireMigrationLock() (func(), error) {
	lockDB, err := sql.Open("mysql", m.dsn())
	if err != nil {
		return nil, fmt.Errorf("open mysql lock connection: %w", err)
	}
	var result int
	if err := lockDB.QueryRow("SELECT GET_LOCK('crosslink_migration', 30)").Scan(&result); err != nil || result != 1 {
		lockDB.Close()
		if err != nil {
			return nil, fmt.Errorf("acquire mysql migration lock: %w", err)
		}
		return nil, fmt.Errorf("mysql migration lock contention")
	}
	return func() {
		lockDB.Exec("SELECT RELEASE_LOCK('crosslink_migration')")
		lockDB.Close()
	}, nil
}

// EnsureMonthlyPartitions creates monthly partitions using ALTER TABLE ... ADD PARTITION
// with DATETIME(3) boundaries. Skips partitions that already exist.
func (m *MySQLDialect) EnsureMonthlyPartitions(ctx context.Context, db *gorm.DB, table string, months int) error {
	now := time.Now()
	for i := 0; i < months; i++ {
		t := now.AddDate(0, i, 0)
		next := t.AddDate(0, 1, 0)
		partName := fmt.Sprintf("p_%d_%02d", t.Year(), t.Month())
		sql := fmt.Sprintf(
			"ALTER TABLE %s ADD PARTITION (PARTITION %s VALUES LESS THAN ('%s'))",
			table, partName, next.Format("2006-01-02 15:04:05.000"),
		)
		if err := db.WithContext(ctx).Exec(sql).Error; err != nil {
			errMsg := strings.ToLower(err.Error())
			if !strings.Contains(errMsg, "duplicate") && !strings.Contains(errMsg, "already exists") {
				return fmt.Errorf("add partition %s: %w", partName, err)
			}
		}
	}
	return nil
}

// DateTrunc returns a date truncation expression for MySQL.
func (m *MySQLDialect) DateTrunc(granularity string, column string) string {
	switch granularity {
	case "hour":
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:00:00')", column)
	default:
		return fmt.Sprintf("DATE(%s)", column)
	}
}

// DateFormat returns a DATE_FORMAT expression for MySQL.
func (m *MySQLDialect) DateFormat(column string, format string) string {
	return fmt.Sprintf("DATE_FORMAT(%s, '%s')", column, format)
}

// ILike returns a LIKE expression for MySQL (LIKE is case-insensitive by default with utf8mb4).
func (m *MySQLDialect) ILike(column string, param string) string {
	return fmt.Sprintf("%s LIKE %s", column, param)
}

// JSONMergePatch returns a JSON_MERGE_PATCH expression for MySQL.
func (m *MySQLDialect) JSONMergePatch(column string, jsonExpr string) string {
	return fmt.Sprintf("JSON_MERGE_PATCH(COALESCE(%s, '{}'), %s)", column, jsonExpr)
}

// ConditionalCount returns a SUM(CASE WHEN ...) expression for conditional counting.
func (m *MySQLDialect) ConditionalCount(column string, value string) string {
	return fmt.Sprintf("SUM(CASE WHEN %s = %s THEN 1 ELSE 0 END)", column, value)
}

// CastFloat returns a MySQL float cast expression.
func (m *MySQLDialect) CastFloat(expr string) string {
	return "CAST(" + expr + " AS DOUBLE)"
}

// Shutdown closes the underlying SQL database connection.
func (m *MySQLDialect) Shutdown(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// dsn returns the MySQL connection string.
func (m *MySQLDialect) dsn() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true",
		m.cfg.User, m.cfg.Password, m.cfg.Host, m.cfg.Port, m.cfg.DBName)
}
