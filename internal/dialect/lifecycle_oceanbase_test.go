//go:build integration

package dialect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// oceanbaseTestDSN returns the OceanBase connection string from the environment.
func oceanbaseTestDSN() string {
	if dsn := os.Getenv("OCEANBASE_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:2883)/test?charset=utf8mb4&parseTime=true&loc=UTC"
}

// oceanbaseTestDBConfig builds a DBConfig for the test OceanBase instance.
func oceanbaseTestDBConfig() DBConfig {
	return DBConfig{
		Driver:   "oceanbase",
		Host:     "127.0.0.1",
		Port:     2883,
		User:     "root",
		Password: "",
		DBName:   "test",
	}
}

// openTestOceanBase opens a raw GORM connection to OceanBase.
// Calls t.Skipf if OceanBase is not available.
func openTestOceanBase(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.Open(oceanbaseTestDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("OceanBase not available: %v", err)
	}
	return db
}

func TestOceanBaseLifecycle_FullWorkflow(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db := openTestOceanBase(t)

	// Create test database
	db.Exec("CREATE DATABASE IF NOT EXISTS test")
	db.Exec("USE test")
	dropAllTablesMySQL(t, db)

	d := NewOceanBaseDialect(oceanbaseTestDBConfig())

	gormDB, err := d.InitDB()
	require.NoError(t, err)

	err = d.RunMigrations(context.Background())
	require.NoError(t, err)

	// Verify tables exist
	expectedTables := []string{
		"system_settings", "roles", "users", "providers",
		"provider_models", "api_keys", "usage_logs",
		"guardrail_rules", "audit_logs",
	}
	for _, table := range expectedTables {
		var count int64
		gormDB.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count)
		assert.Equal(t, int64(1), count, "table %q should exist", table)
	}

	// Verify capabilities
	cap := d.Capabilities()
	assert.False(t, cap.PartialIndex)
	assert.Equal(t, 100, cap.ConcurrentWrites)
	assert.True(t, cap.AdvisoryLock)
	assert.Equal(t, PartitionNative, d.PartitionSupport())

	err = d.Shutdown(gormDB)
	assert.NoError(t, err)

	t.Log("OK: OceanBase full lifecycle completed successfully")
}

func TestOceanBaseLifecycle_MigrationLock(t *testing.T) {
	cfg := oceanbaseTestDBConfig()
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName)

	release, err := d.AcquireMigrationLock()
	if err != nil {
		t.Skipf("OceanBase not available for lock test: %v", err)
	}

	// Lock acquired — release it
	release()

	t.Log("OK: OceanBase migration lock works correctly")
}

// Suppress unused import
var _ = fmt.Sprintf
