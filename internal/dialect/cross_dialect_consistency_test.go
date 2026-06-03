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
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// dialectUnderTest holds a dialect instance, its DB connection, and cleanup.
type dialectUnderTest struct {
	name    string
	dia     Dialect
	db      *gorm.DB
	cleanup func()
}

// getAvailableDialects returns all dialects available for testing.
// SQLite is always available. PG and MySQL are included only if reachable.
func getAvailableDialects(t *testing.T) []dialectUnderTest {
	t.Helper()

	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)

	var dialects []dialectUnderTest

	// Always available: SQLite in-memory
	{
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		db.Exec("PRAGMA foreign_keys = ON")

		schemaBytes, err := os.ReadFile(filepath.Join("migrations", "sqlite", "000001_init_schema.up.sql"))
		require.NoError(t, err)
		for _, stmt := range splitSQL(string(schemaBytes)) {
			if stmt != "" {
				require.NoError(t, db.Exec(stmt).Error)
			}
		}

		dialects = append(dialects, dialectUnderTest{
			name: "sqlite",
			dia:  &SQLiteDialect{},
			db:   db,
			cleanup: func() {
				sqlDB, _ := db.DB()
				sqlDB.Close()
			},
		})
	}

	// Optionally: PostgreSQL
	{
		dsn := os.Getenv("PG_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://crosslink:crosslink_test@localhost:5433/crosslink_test_pg?sslmode=disable"
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err == nil {
			db.Exec("DROP SCHEMA public CASCADE")
			db.Exec("CREATE SCHEMA public")

			d := NewPostgresDialect(pgTestDBConfig())
			gormDB, err2 := d.InitDB()
			if err2 == nil {
				if err2 = d.RunMigrations(t.Context()); err2 == nil {
					dialects = append(dialects, dialectUnderTest{
						name: "postgres",
						dia:  d,
						db:   gormDB,
						cleanup: func() {
							d.Shutdown(gormDB)
						},
					})
				}
			}
		}
	}

	// Optionally: MySQL
	{
		dsn := os.Getenv("MYSQL_TEST_DSN")
		if dsn == "" {
			dsn = "root:crosslink_test@tcp(127.0.0.1:3307)/crosslink_test_mysql?charset=utf8mb4&parseTime=true&loc=UTC"
		}
		db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err == nil {
			dropAllTablesMySQL(t, db)

			d := NewMySQLDialect(mysqlTestDBConfig())
			gormDB, err2 := d.InitDB()
			if err2 == nil {
				if err2 = d.RunMigrations(t.Context()); err2 == nil {
					dialects = append(dialects, dialectUnderTest{
						name: "mysql",
						dia:  d,
						db:   gormDB,
						cleanup: func() {
							d.Shutdown(gormDB)
						},
					})
				}
			}
		}
	}

	return dialects
}

func TestCrossDialect_DateTruncConsistency(t *testing.T) {
	dialects := getAvailableDialects(t)
	for _, d := range dialects {
		defer d.cleanup()
	}

	ts := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)

	type bucketResult struct {
		Bucket string
		Count  int64
	}
	results := make(map[string][]bucketResult)

	for _, d := range dialects {
		for i := 0; i < 3; i++ {
			insertTestUsageLogAny(t, d.db, ts.Add(time.Duration(i)*time.Minute), 0.01)
		}

		expr := d.dia.DateTrunc("hour", "created_at")
		var buckets []struct {
			Bucket string `gorm:"column:bucket"`
			Count  int64  `gorm:"column:cnt"`
		}
		err := d.db.Raw(
			fmt.Sprintf("SELECT %s as bucket, COUNT(*) as cnt FROM usage_logs GROUP BY bucket", expr),
		).Scan(&buckets).Error
		require.NoError(t, err, "%s: DateTrunc GROUP BY failed", d.name)

		for _, b := range buckets {
			results[d.name] = append(results[d.name], bucketResult{Bucket: b.Bucket, Count: b.Count})
		}
	}

	for _, d := range dialects {
		require.Len(t, results[d.name], 1, "%s: expected 1 bucket, got %d", d.name, len(results[d.name]))
		assert.Equal(t, int64(3), results[d.name][0].Count, "%s: bucket count", d.name)
	}

	t.Logf("OK: DateTrunc consistency across %d dialect(s)", len(dialects))
}

func TestCrossDialect_DateFormatConsistency(t *testing.T) {
	dialects := getAvailableDialects(t)
	for _, d := range dialects {
		defer d.cleanup()
	}

	ts := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	results := make(map[string]string)

	for _, d := range dialects {
		insertTestUsageLogAny(t, d.db, ts, 0.05)

		expr := d.dia.DateFormat("created_at", "%Y-%m")
		var month string
		err := d.db.Raw(fmt.Sprintf("SELECT %s FROM usage_logs LIMIT 1", expr)).Scan(&month).Error
		require.NoError(t, err, "%s: DateFormat query failed", d.name)
		results[d.name] = month
	}

	for _, d := range dialects {
		assert.Equal(t, "2026-06", results[d.name], "%s: DateFormat result", d.name)
	}

	t.Logf("OK: DateFormat consistency across %d dialect(s)", len(dialects))
}

func TestCrossDialect_ILikeConsistency(t *testing.T) {
	dialects := getAvailableDialects(t)
	for _, d := range dialects {
		defer d.cleanup()
	}

	results := make(map[string]int64)

	for _, d := range dialects {
		require.NoError(t, d.db.Exec(
			"INSERT INTO agent_fingerprints (name, source_type, source_field, pattern, risk_level, origin, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
			"TestAgent", "header", "user-agent", "%test%", "medium", "manual", "active",
		).Error)

		expr := d.dia.ILike("name", "?")
		var count int64
		err := d.db.Raw(
			fmt.Sprintf("SELECT count(*) FROM agent_fingerprints WHERE %s", expr),
			"%test%",
		).Scan(&count).Error
		require.NoError(t, err)
		results[d.name] = count
	}

	for _, d := range dialects {
		assert.Equal(t, int64(1), results[d.name], "%s: ILike should match 1 row", d.name)
	}

	t.Logf("OK: ILike consistency across %d dialect(s)", len(dialects))
}

func TestCrossDialect_JSONMergePatchConsistency(t *testing.T) {
	dialects := getAvailableDialects(t)
	for _, d := range dialects {
		defer d.cleanup()
	}

	results := make(map[string]map[string]interface{})

	for _, d := range dialects {
		require.NoError(t, dbInsertProvider(d.db, "test-provider", `{"a":1}`))

		patchExpr := d.dia.JSONMergePatch("extra_config", `'{"b":2}'`)
		err := d.db.Exec(
			fmt.Sprintf("UPDATE providers SET extra_config = %s WHERE name = ?", patchExpr),
			"test-provider",
		).Error
		require.NoError(t, err, "%s: JSONMergePatch failed", d.name)

		var extraConfig string
		d.db.Raw("SELECT extra_config FROM providers WHERE name = ?", "test-provider").Scan(&extraConfig)

		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(extraConfig), &parsed), "%s: JSON parse failed", d.name)
		results[d.name] = parsed
	}

	for _, d := range dialects {
		assert.Equal(t, float64(1), results[d.name]["a"], "%s: original key 'a' should be preserved", d.name)
		assert.Equal(t, float64(2), results[d.name]["b"], "%s: new key 'b' should be added", d.name)
	}

	t.Logf("OK: JSONMergePatch consistency across %d dialect(s)", len(dialects))
}

// --- Cross-dialect data insertion helpers ---

func insertTestUsageLogAny(t *testing.T, db *gorm.DB, ts time.Time, cost float64) {
	t.Helper()
	switch db.Dialector.Name() {
	case "sqlite":
		db.Exec(
			"INSERT INTO usage_logs (request_id, model_requested, model_used, route_type, status_code, cost, latency_ms, input_tokens, output_tokens, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			fmt.Sprintf("req-%d", ts.UnixNano()), "gpt-4", "gpt-4", "weighted", 200, cost, 100, 50, 100, ts.Format("2006-01-02 15:04:05"),
		)
	default:
		db.Exec(
			"INSERT INTO usage_logs (request_id, model_requested, model_used, route_type, status_code, cost, latency_ms, input_tokens, output_tokens, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			fmt.Sprintf("req-%d", ts.UnixNano()), "gpt-4", "gpt-4", "weighted", 200, cost, 100, 50, 100, ts,
		)
	}
}

func dbInsertProvider(db *gorm.DB, name, extraConfig string) error {
	return db.Exec(
		"INSERT INTO providers (name, display_name, adapter_type, base_url, api_key, extra_config, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
		name, "Test", "openai_compatible", "https://api.example.com", "sk-test", extraConfig, 1,
	).Error
}
