package dialect

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func testOceanBaseDialect() *OceanBaseDialect {
	return NewOceanBaseDialect(DBConfig{
		Driver:   "oceanbase",
		Host:     "localhost",
		Port:     2883,
		User:     "root",
		Password: "test",
		DBName:   "crosslink",
	})
}

func TestOceanBaseDialect_Identity(t *testing.T) {
	d := testOceanBaseDialect()
	assert.Equal(t, "oceanbase", d.Name())
	assert.Equal(t, "migrations/mysql", d.MigrationDir())
}

func TestOceanBaseDialect_Capabilities(t *testing.T) {
	d := testOceanBaseDialect()
	cap := d.Capabilities()
	assert.False(t, cap.PartialIndex, "OceanBase should not support partial indexes")
	assert.Equal(t, 100, cap.ConcurrentWrites)
	assert.True(t, cap.AdvisoryLock, "OceanBase should support GET_LOCK")
}

func TestOceanBaseDialect_PoolConfig(t *testing.T) {
	d := testOceanBaseDialect()
	pool := d.PoolConfig()
	assert.Equal(t, 100, pool.MaxOpenConns)
	assert.Equal(t, 50, pool.MaxIdleConns)
}

func TestOceanBaseDialect_PartitionSupport(t *testing.T) {
	d := testOceanBaseDialect()
	assert.Equal(t, PartitionNative, d.PartitionSupport())
}

func TestOceanBaseDialect_SQLHelpers(t *testing.T) {
	d := testOceanBaseDialect()

	// DateTrunc — same as MySQL
	assert.Equal(t, "DATE(created_at)", d.DateTrunc("day", "created_at"))
	assert.Equal(t, "DATE_FORMAT(created_at, '%Y-%m-%d %H:00:00')", d.DateTrunc("hour", "created_at"))

	// DateFormat — same as MySQL
	assert.Equal(t, "DATE_FORMAT(created_at, '%Y-%m')", d.DateFormat("created_at", "%Y-%m"))

	// ILike — same as MySQL (LIKE is case-insensitive with utf8mb4)
	assert.Equal(t, "name LIKE ?", d.ILike("name", "?"))

	// JSONMergePatch — same as MySQL
	assert.Equal(t, "JSON_MERGE_PATCH(COALESCE(extra_config, '{}'), '{\"key\":\"val\"}')",
		d.JSONMergePatch("extra_config", "'{\"key\":\"val\"}'"))
}

func TestOceanBaseDialect_Interface(t *testing.T) {
	var _ Dialect = NewOceanBaseDialect(DBConfig{})
}
