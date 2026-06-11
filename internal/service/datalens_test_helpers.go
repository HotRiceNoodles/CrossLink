//go:build integration

package service

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const testOrgID = -1

// testDB connects to a PostgreSQL instance for integration tests.
// Uses env vars: TEST_PG_HOST (default localhost), TEST_PG_PORT (5432),
// TEST_PG_USER (postgres), TEST_PG_PASSWORD (postgres), TEST_PG_DBNAME (llm_gateway_test)
func testDB(t *testing.T) (*gorm.DB, dialect.Dialect, func()) {
	t.Helper()

	cfg := dialect.DBConfig{
		Driver:   "postgres",
		Host:     envOrDefault("TEST_PG_HOST", "localhost"),
		Port:     envIntOrDefault("TEST_PG_PORT", 5432),
		User:     envOrDefault("TEST_PG_USER", "postgres"),
		Password: envOrDefault("TEST_PG_PASSWORD", "postgres"),
		DBName:   envOrDefault("TEST_PG_DBNAME", "llm_gateway_test"),
		SSLMode:  envOrDefault("TEST_PG_SSLMODE", "disable"),
		Timezone: "UTC",
	}

	d := dialect.NewPostgresDialect(cfg)
	db, err := d.InitDB()
	require.NoError(t, err, "failed to connect to test database")

	// Silence GORM logging in tests.
	db.Logger = logger.Default.LogMode(logger.Silent)

	// Reset schema to clean state.
	db.Exec("DROP SCHEMA public CASCADE")
	db.Exec("CREATE SCHEMA public")

	// Run migrations from project root.
	projectRoot := filepath.Join("..", "..")
	t.Chdir(projectRoot)
	require.NoError(t, d.RunMigrations(context.Background()), "migrations failed")

	cleanup := func() { d.Shutdown(db) }
	return db, d, cleanup
}

// logOption customizes generated UsageLog rows.
type logOption func(*model.UsageLog)

// withModel sets the model name.
func withModel(name string) logOption {
	return func(l *model.UsageLog) { l.ModelRequested = name }
}

// withCost sets the cost.
func withCost(cost float64) logOption {
	return func(l *model.UsageLog) { l.Cost = cost }
}

// withTeamID sets the team.
func withTeamID(id int64) logOption {
	return func(l *model.UsageLog) { l.TeamID = &id }
}

// withStatusCode sets the status code.
func withStatusCode(code int) logOption {
	return func(l *model.UsageLog) { l.StatusCode = code }
}

// withLatency sets the latency in ms.
func withLatency(ms int) logOption {
	return func(l *model.UsageLog) { l.LatencyMs = ms }
}

// withTokens sets input and output token counts.
func withTokens(in, out int) logOption {
	return func(l *model.UsageLog) { l.InputTokens = in; l.OutputTokens = out }
}

// withCurrency sets the currency.
func withCurrency(c string) logOption {
	return func(l *model.UsageLog) { l.Currency = c }
}

// insertTestLogs inserts N usage_log rows for testing. Rows are evenly spread
// across the time range [start, start+duration). If opts are provided, they are
// applied to every row (use logOptions for per-row customization).
func insertTestLogs(t *testing.T, db *gorm.DB, orgID int64, start time.Time, count int, opts ...logOption) {
	t.Helper()

	rng := rand.New(rand.NewSource(42))
	models := []string{"gpt-4o", "claude-3.5-sonnet", "gpt-4o-mini", "deepseek-v3"}
	batchSize := 50

	logs := make([]model.UsageLog, 0, batchSize)
	for i := 0; i < count; i++ {
		// Spread timestamps across a 1-hour window by default.
		offset := time.Duration(float64(time.Hour) * float64(i) / float64(count))
		ts := start.Add(offset)

		cost := 0.001 + rng.Float64()*0.05
		latency := 100 + rng.Intn(900)
		inTokens := 50 + rng.Intn(950)
		outTokens := 20 + rng.Intn(480)

		org := orgID
		l := model.UsageLog{
			RequestID:      fmt.Sprintf("req-test-%d-%d", orgID, i),
			OrgID:          &org,
			ModelRequested: models[rng.Intn(len(models))],
			ModelUsed:      models[rng.Intn(len(models))],
			InputTokens:    inTokens,
			OutputTokens:   outTokens,
			Cost:           cost,
			LatencyMs:      latency,
			StatusCode:     200,
			Currency:       "CNY",
			RouteType:      "weighted",
			CreatedAt:      ts,
		}

		for _, opt := range opts {
			opt(&l)
		}

		logs = append(logs, l)
		if len(logs) == batchSize {
			require.NoError(t, db.CreateInBatches(logs, batchSize).Error, "failed to insert test logs")
			logs = logs[:0]
		}
	}
	if len(logs) > 0 {
		require.NoError(t, db.CreateInBatches(logs, batchSize).Error, "failed to insert test logs")
	}
}

// insertHourlyMetrics inserts N hourly metric rows for the given org, spread
// across consecutive hours starting from hour0.
func insertHourlyMetrics(t *testing.T, db *gorm.DB, orgID int64, hour0 time.Time, hours int, baseRequests int, baseCost float64) {
	t.Helper()

	truncated := time.Date(hour0.Year(), hour0.Month(), hour0.Day(), hour0.Hour(), 0, 0, 0, time.UTC)
	rows := make([]model.DataLensHourlyMetric, 0, 50)
	for i := 0; i < hours; i++ {
		bucket := truncated.Add(time.Duration(i) * time.Hour)
		latency := 100 + i*10
		rows = append(rows, model.DataLensHourlyMetric{
			OrgID:          orgID,
			AggLevel:       "global",
			HourBucket:     bucket,
			Currency:       "CNY",
			RequestCount:   baseRequests + i,
			InputTokens:    int64(1000 + i*100),
			OutputTokens:   int64(500 + i*50),
			TotalCost:      baseCost + float64(i)*0.1,
			TotalLatencyMs: int64(latency * (baseRequests + i)),
			MinLatencyMs:   50 + i,
			MaxLatencyMs:   500 + i*20,
			LatencySamples: baseRequests + i,
			StatusGroup:    200,
		})
		if len(rows) == 50 {
			require.NoError(t, db.CreateInBatches(rows, 50).Error)
			rows = rows[:0]
		}
	}
	if len(rows) > 0 {
		require.NoError(t, db.CreateInBatches(rows, 50).Error)
	}
}

// cleanupData removes all test data for the given org across all DataLens tables.
func cleanupData(t *testing.T, db *gorm.DB, orgID int64) {
	t.Helper()
	db.Exec("DELETE FROM datalens_hourly_metrics WHERE org_id = ?", orgID)
	db.Exec("DELETE FROM datalens_daily_metrics WHERE org_id = ?", orgID)
	db.Exec("DELETE FROM datalens_agg_status WHERE agg_level LIKE 'test_%'")
	db.Exec("DELETE FROM usage_logs WHERE org_id = ?", orgID)
}

// envOrDefault reads an env var or returns the fallback.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envIntOrDefault reads an env var as int or returns the fallback.
func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
