package service

// U-11 regression: the DataLens aggregation SQL was written PG-only ($N
// placeholders, expression-target ON CONFLICT, || concat) and silently did
// nothing on the other dialects. This test runs a FULL aggregation lifecycle
// — backfill day, live cycle, rerun — against in-process SQLite, whose TEXT
// bucket storage is the most fragile of the dialects (driver-bound times
// carry a zone suffix while trunc-written buckets do not; see bucketBound).
// No build tag and no external database: glebarez :memory: only.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// testMCPToolCallLog mirrors the columns of mcp.MCPToolCallLog that the
// rollup reads (the aggregator addresses the table by name only). The model
// itself can't be imported: internal/mcp transitively imports
// internal/service, an import cycle for an in-package test.
type testMCPToolCallLog struct {
	ID         int64     `gorm:"primaryKey"`
	OrgID      *int64    `gorm:"index"`
	RequestID  string    `gorm:"size:36"`
	ServerName string    `gorm:"size:64"`
	ToolName   string    `gorm:"size:128"`
	Method     string    `gorm:"size:32"`
	Duration   int       `gorm:"default:0"`
	Status     int16     `gorm:"not null"` // 1=success, 0=error, -1=blocked
	APIKeyID   int64     `gorm:"index"`
	CreatedAt  time.Time `gorm:"not null"`
}

func (testMCPToolCallLog) TableName() string { return "mcp_tool_call_logs" }

// newAggSQLite opens a per-test in-memory SQLite (shared-cache name so every
// pooled connection sees the same database; single conn so the replaceWindow
// transactions never fight SQLite's write lock).
func newAggSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.UsageLog{},
		&model.DataLensHourlyMetric{},
		&model.DataLensDailyMetric{},
		&model.DataLensToolDailyMetric{},
		&model.DataLensAggStatus{},
		&model.AgentToolsetEvent{},
		&testMCPToolCallLog{},
	))
	return db
}

type aggSeedTimes struct {
	y14   time.Time // yesterday 14:05 — bulk of the data
	y15   time.Time // yesterday 15:10 — second hourly bucket
	today time.Time // now-30m — inside the rolling 3h window
}

func seedAggData(t *testing.T, db *gorm.DB) aggSeedTimes {
	t.Helper()
	now := time.Now().UTC()
	y := now.AddDate(0, 0, -1)
	ts := aggSeedTimes{
		y14:   time.Date(y.Year(), y.Month(), y.Day(), 14, 5, 0, 0, time.UTC),
		y15:   time.Date(y.Year(), y.Month(), y.Day(), 15, 10, 0, 0, time.UTC),
		today: now.Add(-30 * time.Minute),
	}

	org := int64(1)
	key := int64(7)
	team := int64(3)
	prov := int64(2)

	// usage_logs: 5 rows for org 1 across two of yesterday's hours + 1 today.
	usage := []model.UsageLog{
		{RequestID: "u1", APIKeyID: &key, ProviderID: &prov, TeamID: &team, SessionID: "s1",
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 100, OutputTokens: 50, Cost: 0.01, LatencyMs: 120, StatusCode: 200,
			Currency: "CNY", OrgID: &org, CreatedAt: ts.y14},
		{RequestID: "u2", APIKeyID: &key, ProviderID: &prov, TeamID: &team, SessionID: "s1",
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 100, OutputTokens: 50, Cost: 0.01, LatencyMs: 100, StatusCode: 200,
			Currency: "CNY", OrgID: &org, CreatedAt: ts.y14.Add(time.Minute)},
		{RequestID: "u3", APIKeyID: &key, ProviderID: &prov, TeamID: &team, SessionID: "s2",
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 100, OutputTokens: 50, Cost: 0.01, LatencyMs: 150, StatusCode: 200,
			Currency: "CNY", OrgID: &org, CreatedAt: ts.y14.Add(2 * time.Minute)},
		{RequestID: "u4", APIKeyID: &key, ProviderID: &prov, TeamID: &team,
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 20, OutputTokens: 0, Cost: 0.02, LatencyMs: 300, StatusCode: 500,
			Currency: "CNY", OrgID: &org, CreatedAt: ts.y14.Add(30 * time.Minute)},
		{RequestID: "u5", APIKeyID: &key, ProviderID: &prov, TeamID: &team, SessionID: "s2",
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 100, OutputTokens: 50, Cost: 0.04, LatencyMs: 80, StatusCode: 200,
			Currency: "CNY", OrgID: &org, CreatedAt: ts.y15},
		{RequestID: "u6", APIKeyID: &key, ProviderID: &prov, TeamID: &team, SessionID: "s1",
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 100, OutputTokens: 50, Cost: 0.08, LatencyMs: 60, StatusCode: 200,
			Currency: "CNY", OrgID: &org, CreatedAt: ts.today},
		// org-less row: excluded from every aggregate.
		{RequestID: "u7",
			RouteType: "primary", ModelRequested: "gpt-4o", ModelUsed: "gpt-4o",
			InputTokens: 10, OutputTokens: 5, Cost: 99, LatencyMs: 10, StatusCode: 200,
			Currency: "CNY", CreatedAt: ts.y14.Add(3 * time.Minute)},
	}
	for i := range usage {
		require.NoError(t, db.Create(&usage[i]).Error, "usage %d", i)
	}

	// mcp_tool_call_logs: 4 tools/call for (org 1, key 7, db/query) + noise.
	mcpLogs := []testMCPToolCallLog{
		{OrgID: &org, RequestID: "m1", ServerName: "db", ToolName: "query", Method: "tools/call", Duration: 50, Status: 1, APIKeyID: key, CreatedAt: ts.y14.Add(5 * time.Minute)},
		{OrgID: &org, RequestID: "m2", ServerName: "db", ToolName: "query", Method: "tools/call", Duration: 70, Status: 1, APIKeyID: key, CreatedAt: ts.y14.Add(6 * time.Minute)},
		{OrgID: &org, RequestID: "m3", ServerName: "db", ToolName: "query", Method: "tools/call", Duration: 999, Status: -1, APIKeyID: key, CreatedAt: ts.y14.Add(7 * time.Minute)},
		{OrgID: &org, RequestID: "m4", ServerName: "db", ToolName: "query", Method: "tools/call", Duration: 500, Status: 0, APIKeyID: key, CreatedAt: ts.y14.Add(8 * time.Minute)},
		// tools/list is discovery traffic, not a tool call.
		{OrgID: &org, RequestID: "m5", ServerName: "db", ToolName: "query", Method: "tools/list", Duration: 10, Status: 1, APIKeyID: key, CreatedAt: ts.y14.Add(9 * time.Minute)},
		// org-less call: excluded.
		{RequestID: "m6", ServerName: "db", ToolName: "query", Method: "tools/call", Duration: 10, Status: 1, APIKeyID: key, CreatedAt: ts.y14.Add(9 * time.Minute)},
	}
	for i := range mcpLogs {
		require.NoError(t, db.Create(&mcpLogs[i]).Error, "mcp %d", i)
	}

	// agent_toolset_events: one of each observed event type for the same tool.
	events := []model.AgentToolsetEvent{
		{OrgID: org, APIKeyID: key, ToolName: "mcp/db/query", EventType: "first_seen", CreatedAt: ts.y14.Add(9 * time.Minute)},
		{OrgID: org, APIKeyID: key, ToolName: "mcp/db/query", EventType: "definition_changed", CreatedAt: ts.y15},
		{OrgID: org, APIKeyID: key, ToolName: "mcp/db/query", EventType: "disappeared", CreatedAt: ts.y15.Add(time.Minute)},
	}
	for i := range events {
		require.NoError(t, db.Create(&events[i]).Error, "event %d", i)
	}
	return ts
}

func TestAggregatorCycleSQLite(t *testing.T) {
	db := newAggSQLite(t)
	ts := seedAggData(t, db)

	d := dialect.NewSQLiteDialect(dialect.DBConfig{})
	svc := NewDataLensAggregatorService(db, d, config.DataLensConfig{
		Agg: config.DataLensAggConfig{BackfillDays: 1}, // yesterday
	})
	assertAggregatorLifecycle(t, db, svc, ts)
}

// assertAggregatorLifecycle runs backfill + one live cycle, asserts the full
// expected aggregate state, then reruns the whole lifecycle to prove
// idempotency. Dialect-agnostic on purpose (shared by the SQLite and MySQL
// runs): buckets are matched via CAST(... AS CHAR) prefixes, which both
// dialects render as 'YYYY-MM-DD[ HH:MM:SS]'.
func assertAggregatorLifecycle(t *testing.T, db *gorm.DB, svc *DataLensAggregatorService, ts aggSeedTimes) {
	t.Helper()
	ctx := context.Background()

	// First lifecycle: backfill yesterday, then one live cycle (today's
	// rolling window + 3-day daily rollup + tool rollup).
	svc.backfill(ctx)
	require.NoError(t, svc.AggregateOnce(ctx))

	yDay := ts.y14.Truncate(24 * time.Hour)

	// Hourly, global level, yesterday. status_group is a metric dimension,
	// so hour 14:00 splits into two rows (status 200 ×3, status 500 ×1) and
	// 15:00 is one row — 3 rows total, 5 requests, 1 error; no org-less rows
	// anywhere.
	var n, req, errs int64
	var cost float64
	db.Raw(`SELECT COUNT(*) FROM datalens_hourly_metrics WHERE org_id IS NULL`).Scan(&n)
	assert.Zero(t, n, "org-less usage must not be aggregated")
	db.Raw(`SELECT COUNT(*) FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&n)
	db.Raw(`SELECT COALESCE(SUM(request_count),0) FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&req)
	db.Raw(`SELECT COALESCE(SUM(error_count),0) FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&errs)
	db.Raw(`SELECT COALESCE(SUM(total_cost),0) FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&cost)
	assert.Equal(t, int64(3), n, "yesterday rows: (14:00,200), (14:00,500), (15:00,200)")
	assert.Equal(t, int64(5), req, "yesterday request_count (4×hour14 + 1×hour15)")
	assert.Equal(t, int64(1), errs, "one 5xx row")
	assert.InDelta(t, 0.09, cost, 1e-9, "yesterday total_cost 0.01×3 + 0.02 + 0.04")

	// Per-bucket split: the 14:00 bucket holds the four rows created in it
	// (summed across its two status-group rows).
	type bucketRow struct {
		Bucket string
		Rc     int64
	}
	var buckets []bucketRow
	db.Raw(`SELECT CAST(hour_bucket AS CHAR) AS bucket, request_count AS rc
		FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&buckets)
	// NB: the hour can't go through Format — "14" in a layout parses as
	// month-token "1" + literal "4" — so the prefix is built by hand.
	y14Prefix := ts.y14.Format("2006-01-02") + " 14:"
	var hour14 int64
	for _, b := range buckets {
		if strings.HasPrefix(b.Bucket, y14Prefix) {
			hour14 += b.Rc
		}
	}
	assert.Equal(t, int64(4), hour14, "14:00 bucket request_count")

	// Daily, global level, yesterday: rolled up from hourly + distinct counts
	// recomputed from raw logs. Queried on the status-200 row (the 500 row
	// carries the lone u4): 4 requests there, sessions {s1,s2}, key {7}.
	var dReq, dSess, dKeys int64
	var dCost float64
	row := db.Raw(`SELECT request_count, distinct_sessions, distinct_keys, total_cost
		FROM datalens_daily_metrics WHERE agg_level='global' AND org_id=1 AND day_bucket=date(?) AND status_group=200`, yDay).Row()
	require.NoError(t, row.Scan(&dReq, &dSess, &dKeys, &dCost))
	assert.Equal(t, int64(4), dReq, "status-200 daily row (u1,u2,u3,u5)")
	assert.Equal(t, int64(2), dSess, "distinct sessions {s1,s2}")
	assert.Equal(t, int64(1), dKeys, "distinct keys {7}")
	assert.InDelta(t, 0.07, dCost, 1e-9, "0.01×3 + 0.04, the 500 row's 0.02 sits in its own row")

	// Tool rollup: exactly one row folding both sources; duration counts
	// success-only samples; every event type lands.
	var tools int64
	db.Raw(`SELECT COUNT(*) FROM datalens_tool_daily_metrics`).Scan(&tools)
	assert.Equal(t, int64(1), tools, "single (org,day,key,tool) row")
	var tool model.DataLensToolDailyMetric
	require.NoError(t, db.First(&tool).Error)
	assert.Equal(t, "mcp/db/query", tool.ToolName)
	assert.Equal(t, int64(4), tool.McpCalls, "m1-m4 (tools/list m5 excluded)")
	assert.Equal(t, int64(1), tool.McpErrors, "status 0")
	assert.Equal(t, int64(1), tool.McpBlocked, "status -1")
	assert.Equal(t, int64(120), tool.TotalDurationMs, "50+70 success-only (blocked/error durations excluded)")
	assert.Equal(t, int64(2), tool.DurationSamples)
	assert.Equal(t, 1, tool.FirstSeenCount)
	assert.Equal(t, 1, tool.DefinitionChangedCount)
	assert.Equal(t, 1, tool.DisappearedCount)

	// Idempotency: the whole lifecycle again must land on identical state —
	// replaceWindow deletes each window before reinserting, so any leftover
	// or duplicated row shows up here.
	svc.backfill(ctx)
	require.NoError(t, svc.AggregateOnce(ctx))
	db.Raw(`SELECT COUNT(*) FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&n)
	db.Raw(`SELECT COALESCE(SUM(request_count),0) FROM datalens_hourly_metrics WHERE agg_level='global' AND org_id=1 AND date(hour_bucket)=date(?)`, yDay).Scan(&req)
	assert.Equal(t, int64(3), n, "rerun: same three rows, no duplicates")
	assert.Equal(t, int64(5), req, "rerun: request sum unchanged")
	db.Raw(`SELECT COUNT(*) FROM datalens_daily_metrics`).Scan(&n)
	assert.Equal(t, int64(14), n, "rerun: 7 levels × 2 status groups, no duplicates")
	db.Raw(`SELECT COUNT(*) FROM datalens_tool_daily_metrics`).Scan(&tools)
	assert.Equal(t, int64(1), tools, "rerun: tool rollup not duplicated")
}
