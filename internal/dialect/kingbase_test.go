package dialect

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func testKingbaseDialect() *KingbaseDialect {
	return NewKingbaseDialect(DBConfig{
		Driver:   "kingbasees",
		Host:     "localhost",
		Port:     54321,
		User:     "kingbase",
		Password: "test",
		DBName:   "crosslink",
		SSLMode:  "disable",
	})
}

func TestKingbaseDialect_Identity(t *testing.T) {
	d := testKingbaseDialect()
	assert.Equal(t, "kingbase", d.Name())
	assert.Equal(t, "migrations/postgres", d.MigrationDir())
}

func TestKingbaseDialect_Capabilities(t *testing.T) {
	d := testKingbaseDialect()
	cap := d.Capabilities()
	assert.True(t, cap.PartialIndex, "KingbaseES should support partial indexes")
	assert.Equal(t, 100, cap.ConcurrentWrites)
	assert.True(t, cap.AdvisoryLock, "KingbaseES should support advisory locks")
}

func TestKingbaseDialect_PoolConfig(t *testing.T) {
	d := testKingbaseDialect()
	pool := d.PoolConfig()
	assert.Equal(t, 100, pool.MaxOpenConns)
	assert.Equal(t, 50, pool.MaxIdleConns)
}

func TestKingbaseDialect_PartitionSupport(t *testing.T) {
	d := testKingbaseDialect()
	assert.Equal(t, PartitionNative, d.PartitionSupport())
}

func TestKingbaseDialect_SQLHelpers(t *testing.T) {
	d := testKingbaseDialect()

	// DateTrunc — should produce same SQL as PG
	assert.Equal(t, "date_trunc('day', created_at)", d.DateTrunc("day", "created_at"))
	assert.Equal(t, "date_trunc('hour', created_at)", d.DateTrunc("hour", "created_at"))

	// DateFormat — same as PG
	assert.Equal(t, "TO_CHAR(created_at, 'YYYY-MM')", d.DateFormat("created_at", "%Y-%m"))

	// ILike — same as PG
	assert.Equal(t, "name ILIKE ?", d.ILike("name", "?"))

	// JSONMergePatch — same as PG
	assert.Equal(t, "COALESCE(extra_config::jsonb, '{}') || '{\"key\":\"val\"}'::jsonb",
		d.JSONMergePatch("extra_config", "'{\"key\":\"val\"}'"))
}

func TestKingbaseDialect_DSNURL(t *testing.T) {
	d := testKingbaseDialect()
	got := d.dsnURL()
	assert.Equal(t, "postgres://kingbase:test@localhost:54321/crosslink?sslmode=disable", got)
}

func TestKingbaseDialect_Interface(t *testing.T) {
	var _ Dialect = NewKingbaseDialect(DBConfig{})
}
