package dialect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupSQLiteTestDB creates an in-memory SQLite DB with the full schema applied.
// Returns the DB and a dialect instance for SQL helper generation.
func setupSQLiteTestDB(t *testing.T) (*gorm.DB, *SQLiteDialect, func()) {
	t.Helper()

	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)

	db.Exec("PRAGMA foreign_keys = ON")

	// Apply full schema
	sqlBytes, err := os.ReadFile(filepath.Join("migrations", "sqlite", "000001_init_schema.up.sql"))
	require.NoError(t, err)

	for _, stmt := range splitSQL(string(sqlBytes)) {
		if stmt == "" {
			continue
		}
		require.NoError(t, db.Exec(stmt).Error, "schema statement failed: %s", truncate(stmt, 100))
	}

	dia := &SQLiteDialect{}
	cleanup := func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}
	return db, dia, cleanup
}

// insertTestUsageLog inserts a usage_logs row with a known timestamp.
func insertTestUsageLog(t *testing.T, db *gorm.DB, ts time.Time, cost float64) {
	t.Helper()
	result := db.Exec(
		"INSERT INTO usage_logs (request_id, model_requested, model_used, route_type, status_code, cost, latency_ms, input_tokens, output_tokens, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		"req-1", "gpt-4", "gpt-4", "weighted", 200, cost, 100, 50, 100, ts.Format("2006-01-02 15:04:05"),
	)
	require.NoError(t, result.Error, "insert usage_logs failed")
}

func TestSQLite_SQLHelpers_DateTrunc(t *testing.T) {
	db, dia, cleanup := setupSQLiteTestDB(t)
	defer cleanup()

	ts := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	insertTestUsageLog(t, db, ts, 0.05)

	tests := []struct {
		name        string
		granularity string
		want        string
	}{
		{"day", "day", "2026-06-15"},
		{"hour", "hour", "2026-06-15 14:00:00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr := dia.DateTrunc(tt.granularity, "created_at")
			var result string
			err := db.Raw(fmt.Sprintf("SELECT %s FROM usage_logs LIMIT 1", expr)).Scan(&result).Error
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
		})
	}
}

func TestSQLite_SQLHelpers_DateFormat(t *testing.T) {
	db, dia, cleanup := setupSQLiteTestDB(t)
	defer cleanup()

	ts := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	insertTestUsageLog(t, db, ts, 0.05)

	var result string
	expr := dia.DateFormat("created_at", "%Y-%m")
	err := db.Raw(fmt.Sprintf("SELECT %s FROM usage_logs LIMIT 1", expr)).Scan(&result).Error
	require.NoError(t, err)
	assert.Equal(t, "2026-06", result)
}

func TestSQLite_SQLHelpers_ILike(t *testing.T) {
	db, dia, cleanup := setupSQLiteTestDB(t)
	defer cleanup()

	// Insert agent_fingerprint with mixed-case name
	require.NoError(t, db.Exec(
		"INSERT INTO agent_fingerprints (name, source_type, source_field, pattern, risk_level, origin, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"TestAgent", "header", "user-agent", "%test%", "medium", "manual", "active",
	).Error)

	// Case-insensitive search via ILike helper
	expr := dia.ILike("name", "?")
	var count int64
	err := db.Raw(
		fmt.Sprintf("SELECT count(*) FROM agent_fingerprints WHERE %s", expr),
		"%test%",
	).Scan(&count).Error
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "ILike should match case-insensitively")

	// Verify uppercase search also works
	var count2 int64
	err = db.Raw(
		fmt.Sprintf("SELECT count(*) FROM agent_fingerprints WHERE %s", expr),
		"%TEST%",
	).Scan(&count2).Error
	require.NoError(t, err)
	assert.Equal(t, int64(1), count2, "ILike should match uppercase pattern")
}

func TestSQLite_SQLHelpers_JSONMergePatch(t *testing.T) {
	db, dia, cleanup := setupSQLiteTestDB(t)
	defer cleanup()

	// Insert a provider with extra_config JSON
	require.NoError(t, db.Exec(
		"INSERT INTO providers (name, display_name, adapter_type, base_url, api_key, extra_config, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"openai", "OpenAI", "openai_compatible", "https://api.openai.com", "sk-test", `{"region":"us"}`, 1,
	).Error)

	// Merge new config using JSONMergePatch helper
	patchExpr := dia.JSONMergePatch("extra_config", `'{"region":"eu","priority":1}'`)
	err := db.Exec(
		fmt.Sprintf("UPDATE providers SET extra_config = %s WHERE name = ?", patchExpr),
		"openai",
	).Error
	require.NoError(t, err)

	// Read back and verify merged result
	var extraConfig string
	db.Raw("SELECT extra_config FROM providers WHERE name = ?", "openai").Scan(&extraConfig)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(extraConfig), &result))
	assert.Equal(t, "eu", result["region"], "JSON merge should update existing key")
	assert.Equal(t, float64(1), result["priority"], "JSON merge should add new key")
}

func TestSQLite_SQLHelpers_DateTrunc_GroupBy(t *testing.T) {
	db, dia, cleanup := setupSQLiteTestDB(t)
	defer cleanup()

	// Insert 10 rows across 3 hours
	baseTime := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	insertCounts := []struct {
		hour  int // offset from baseTime
		count int
	}{
		{0, 4}, // 10:00 — 4 rows
		{1, 3}, // 11:00 — 3 rows
		{2, 3}, // 12:00 — 3 rows
	}
	for _, ic := range insertCounts {
		ts := baseTime.Add(time.Duration(ic.hour) * time.Hour).Add(15 * time.Minute)
		for i := 0; i < ic.count; i++ {
			insertTestUsageLog(t, db, ts, float64(i)*0.01)
		}
	}

	// GROUP BY DateTrunc("hour", ...)
	expr := dia.DateTrunc("hour", "created_at")
	type bucket struct {
		Bucket string `gorm:"column:bucket"`
		Count  int64  `gorm:"column:cnt"`
	}
	var buckets []bucket
	err := db.Raw(
		fmt.Sprintf("SELECT %s as bucket, COUNT(*) as cnt FROM usage_logs GROUP BY bucket ORDER BY bucket", expr),
	).Scan(&buckets).Error
	require.NoError(t, err)

	require.Len(t, buckets, 3)
	assert.Equal(t, "2026-06-15 10:00:00", buckets[0].Bucket)
	assert.Equal(t, int64(4), buckets[0].Count)
	assert.Equal(t, "2026-06-15 11:00:00", buckets[1].Bucket)
	assert.Equal(t, int64(3), buckets[1].Count)
	assert.Equal(t, "2026-06-15 12:00:00", buckets[2].Bucket)
	assert.Equal(t, int64(3), buckets[2].Count)
}
