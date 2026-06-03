//go:build integration

package dialect

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupPGTestDB opens a PG connection, drops all tables, runs migrations.
func setupPGTestDB(t *testing.T) (*gorm.DB, *PostgresDialect, func()) {
	t.Helper()

	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db, err := gorm.Open(postgres.Open(pgTestDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("PostgreSQL not available: %v", err)
	}

	dropAllTablesPG(t, db)

	d := NewPostgresDialect(pgTestDBConfig())
	gormDB, err := d.InitDB()
	if err != nil {
		t.Skipf("PostgreSQL not available: %v", err)
	}
	require.NoError(t, d.RunMigrations(t.Context()))

	cleanup := func() { d.Shutdown(gormDB) }
	return gormDB, d, cleanup
}

// insertPGUsageLog inserts a usage_logs row with a known timestamp.
func insertPGUsageLog(t *testing.T, db *gorm.DB, ts time.Time, cost float64) {
	t.Helper()
	err := db.Exec(
		"INSERT INTO usage_logs (request_id, model_requested, model_used, route_type, status_code, cost, latency_ms, input_tokens, output_tokens, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		fmt.Sprintf("req-%d", ts.UnixNano()), "gpt-4", "gpt-4", "weighted", 200, cost, 100, 50, 100, ts,
	).Error
	require.NoError(t, err)
}

func TestPostgres_SQLHelpers_DateTrunc(t *testing.T) {
	db, dia, cleanup := setupPGTestDB(t)
	defer cleanup()

	ts := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	insertPGUsageLog(t, db, ts, 0.05)

	tests := []struct {
		name        string
		granularity string
		want        string
	}{
		{"day", "day", "2026-06-15T00:00:00Z"},
		{"hour", "hour", "2026-06-15T14:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr := dia.DateTrunc(tt.granularity, "created_at")
			var result time.Time
			err := db.Raw(fmt.Sprintf("SELECT %s FROM usage_logs LIMIT 1", expr)).Scan(&result).Error
			require.NoError(t, err)
			assert.Equal(t, tt.want, result.UTC().Format(time.RFC3339))
		})
	}
}

func TestPostgres_SQLHelpers_DateFormat(t *testing.T) {
	db, dia, cleanup := setupPGTestDB(t)
	defer cleanup()

	ts := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	insertPGUsageLog(t, db, ts, 0.05)

	var result string
	expr := dia.DateFormat("created_at", "%Y-%m")
	err := db.Raw(fmt.Sprintf("SELECT %s FROM usage_logs LIMIT 1", expr)).Scan(&result).Error
	require.NoError(t, err)
	assert.Equal(t, "2026-06", result)
}

func TestPostgres_SQLHelpers_ILike(t *testing.T) {
	db, dia, cleanup := setupPGTestDB(t)
	defer cleanup()

	require.NoError(t, db.Exec(
		"INSERT INTO agent_fingerprints (name, source_type, source_field, pattern, risk_level, origin, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"TestAgent", "header", "user-agent", "%test%", "medium", "manual", "active",
	).Error)

	expr := dia.ILike("name", "?")
	var count int64
	err := db.Raw(
		fmt.Sprintf("SELECT count(*) FROM agent_fingerprints WHERE %s", expr),
		"%test%",
	).Scan(&count).Error
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "ILike should match case-insensitively")

	var count2 int64
	err = db.Raw(
		fmt.Sprintf("SELECT count(*) FROM agent_fingerprints WHERE %s", expr),
		"%TEST%",
	).Scan(&count2).Error
	require.NoError(t, err)
	assert.Equal(t, int64(1), count2, "ILike should match uppercase pattern")
}

func TestPostgres_SQLHelpers_JSONMergePatch(t *testing.T) {
	db, dia, cleanup := setupPGTestDB(t)
	defer cleanup()

	require.NoError(t, db.Exec(
		"INSERT INTO providers (name, display_name, adapter_type, base_url, api_key, extra_config, status) VALUES (?, ?, ?, ?, ?, ?::jsonb, ?)",
		"openai", "OpenAI", "openai_compatible", "https://api.openai.com", "sk-test", `{"region":"us"}`, 1,
	).Error)

	patchExpr := dia.JSONMergePatch("extra_config", `'{"region":"eu","priority":1}'`)
	err := db.Exec(
		fmt.Sprintf("UPDATE providers SET extra_config = %s WHERE name = ?", patchExpr),
		"openai",
	).Error
	require.NoError(t, err)

	var extraConfig string
	db.Raw("SELECT extra_config::text FROM providers WHERE name = ?", "openai").Scan(&extraConfig)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(extraConfig), &result))
	assert.Equal(t, "eu", result["region"])
	assert.Equal(t, float64(1), result["priority"])
}

func TestPostgres_SQLHelpers_DateTrunc_GroupBy(t *testing.T) {
	db, dia, cleanup := setupPGTestDB(t)
	defer cleanup()

	baseTime := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	insertCounts := []struct {
		hour  int
		count int
	}{
		{0, 4},
		{1, 3},
		{2, 3},
	}
	for _, ic := range insertCounts {
		ts := baseTime.Add(time.Duration(ic.hour) * time.Hour).Add(15 * time.Minute)
		for i := 0; i < ic.count; i++ {
			insertPGUsageLog(t, db, ts, float64(i)*0.01)
		}
	}

	expr := dia.DateTrunc("hour", "created_at")
	type bucket struct {
		Bucket time.Time `gorm:"column:bucket"`
		Count  int64     `gorm:"column:cnt"`
	}
	var buckets []bucket
	err := db.Raw(
		fmt.Sprintf("SELECT %s as bucket, COUNT(*) as cnt FROM usage_logs GROUP BY bucket ORDER BY bucket", expr),
	).Scan(&buckets).Error
	require.NoError(t, err)

	require.Len(t, buckets, 3)
	assert.Equal(t, int64(4), buckets[0].Count)
	assert.Equal(t, int64(3), buckets[1].Count)
	assert.Equal(t, int64(3), buckets[2].Count)
}
