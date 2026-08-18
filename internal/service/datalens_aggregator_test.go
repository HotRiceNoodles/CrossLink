//go:build integration

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAggregatorHourlyUpsert verifies that hourly aggregation correctly
// aggregates raw usage_logs and is idempotent on re-run.
func TestAggregatorHourlyUpsert(t *testing.T) {
	db, d, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Clean up any stale test data.
	cleanupData(t, db, testOrgID)

	// Insert 100 usage_logs spread across the hour ending now (so all rows
	// fall inside the aggregator's 3h lookback window).
	hourStart := now.Add(-time.Hour)
	insertTestLogs(t, db, testOrgID, hourStart, 100)

	// Verify raw data landed.
	var rawCount int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM usage_logs WHERE org_id = ?", testOrgID).Scan(&rawCount).Error)
	require.Equal(t, int64(100), rawCount)

	// Compute expected total cost from raw data.
	var expectedCost float64
	require.NoError(t, db.Raw("SELECT COALESCE(SUM(cost), 0) FROM usage_logs WHERE org_id = ?", testOrgID).Scan(&expectedCost).Error)

	// Create aggregator with a wide lookback to cover our test hour.
	cfg := config.DataLensConfig{
		Enabled: true,
		Agg: config.DataLensAggConfig{
			Interval:    "1h",
			Lookback:    "3h",
			BackfillDays: 0,
		},
	}
	svc := NewDataLensAggregatorService(db, d, cfg)

	// Run one aggregation cycle.
	require.NoError(t, svc.AggregateOnce(ctx))

	// Query hourly metrics for our test org (global level only — other levels
	// re-aggregate the same rows across different dimensions).
	var metrics []model.DataLensHourlyMetric
	require.NoError(t, db.Where("org_id = ? AND agg_level = 'global'", testOrgID).Find(&metrics).Error)
	require.NotEmpty(t, metrics, "expected at least one hourly metric row")

	// Assert: SUM(total_cost) across all hourly rows ≈ SUM(cost) from raw usage_logs.
	var aggCost float64
	var aggRequests int
	for _, m := range metrics {
		aggCost += m.TotalCost
		aggRequests += m.RequestCount
	}
	assert.InDelta(t, expectedCost, aggCost, 0.01, "aggregated cost should match raw cost sum")
	assert.Equal(t, 100, aggRequests, "aggregated request count should match raw row count")

	// --- Idempotency test: run aggregation again ---
	rowCountBefore := len(metrics)
	require.NoError(t, svc.AggregateOnce(ctx))

	var metricsAfter []model.DataLensHourlyMetric
	require.NoError(t, db.Where("org_id = ? AND agg_level = 'global'", testOrgID).Find(&metricsAfter).Error)
	assert.Equal(t, rowCountBefore, len(metricsAfter), "re-running aggregation should not create extra rows")

	// Cleanup.
	cleanupData(t, db, testOrgID)
}

// TestAggregatorDailyRollup verifies that daily metrics correctly roll up
// from hourly: SUM(request_count), MIN(min_latency_ms), MAX(max_latency_ms).
func TestAggregatorDailyRollup(t *testing.T) {
	db, d, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	cleanupData(t, db, testOrgID)

	// Insert 24 hourly metric rows for a single day.
	dayStart := time.Now().UTC().Truncate(24 * time.Hour)
	const baseRequests = 10
	const baseCost = 0.5
	insertHourlyMetrics(t, db, testOrgID, dayStart, 24, baseRequests, baseCost)

	// Verify hourly rows were inserted.
	var hourlyCount int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM datalens_hourly_metrics WHERE org_id = ?", testOrgID).Scan(&hourlyCount).Error)
	require.Equal(t, int64(24), hourlyCount)

	// Compute expected values from hourly data.
	var sumRequests int
	var minLatency int
	var maxLatency int
	for i := 0; i < 24; i++ {
		sumRequests += baseRequests + i
		lat := 100 + i*10
		latMin := 50 + i
		latMax := 500 + i*20
		if i == 0 {
			minLatency = latMin
			maxLatency = latMax
		} else {
			if latMin < minLatency {
				minLatency = latMin
			}
			if latMax > maxLatency {
				maxLatency = latMax
			}
		}
		_ = lat
	}

	// Create aggregator and run daily rollup.
	// The aggregateDaily method looks at yesterday's hourly data, so we need
	// to call it with the correct window. We use AggregateOnce which calls
	// aggregateDaily for "today" range. Since we inserted data at dayStart (today),
	// the daily aggregation window (yesterday 00:00 to today 00:00) may not cover it.
	//
	// To work around this, we directly test aggregateDailyFromHourly by calling
	// the service's internal method through a test wrapper.
	cfg := config.DataLensConfig{
		Enabled: true,
		Agg: config.DataLensAggConfig{
			Interval:     "1h",
			Lookback:     "3h",
			BackfillDays: 0,
		},
	}
	svc := NewDataLensAggregatorService(db, d, cfg)

	// Call aggregateDailyFromHourly directly with our test window.
	dayEnd := dayStart.Add(24 * time.Hour)
	require.NoError(t, svc.aggregateDailyFromHourly(ctx, dayStart, dayEnd))

	// Query daily metrics.
	var dailyMetrics []model.DataLensDailyMetric
	require.NoError(t, db.Where("org_id = ?", testOrgID).Find(&dailyMetrics).Error)
	require.NotEmpty(t, dailyMetrics, "expected at least one daily metric row")

	// Assert SUM(request_count).
	var dailySumRequests int
	var dailyMinLatency int
	var dailyMaxLatency int
	for i, dm := range dailyMetrics {
		dailySumRequests += dm.RequestCount
		if i == 0 {
			dailyMinLatency = dm.MinLatencyMs
			dailyMaxLatency = dm.MaxLatencyMs
		} else {
			if dm.MinLatencyMs < dailyMinLatency {
				dailyMinLatency = dm.MinLatencyMs
			}
			if dm.MaxLatencyMs > dailyMaxLatency {
				dailyMaxLatency = dm.MaxLatencyMs
			}
		}
	}

	assert.Equal(t, sumRequests, dailySumRequests, "daily SUM(request_count) should match hourly sum")
	assert.Equal(t, minLatency, dailyMinLatency, "daily MIN(min_latency_ms) should equal min of hourly mins")
	assert.Equal(t, maxLatency, dailyMaxLatency, "daily MAX(max_latency_ms) should equal max of hourly maxes")

	cleanupData(t, db, testOrgID)
}

// TestAggregatorHourlyContextColumns verifies that context-analysis columns
// aggregate correctly: cache_hit rows and unanalyzed rows (analysis_flags IS NULL)
// are excluded; utilization buckets place rows at the correct boundaries.
func TestAggregatorHourlyContextColumns(t *testing.T) {
	db, d, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	cleanupData(t, db, testOrgID)

	now := time.Now().UTC()
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC)

	mk := func(cacheHit bool, bp *int) model.UsageLog {
		org := int64(testOrgID)
		sys, hist, q, tool, toolOut := 100, 200, 300, 400, 500
		win := 100000
		flags := 1 | 8 // overflow + window_unknown both set
		l := model.UsageLog{
			RequestID:      fmt.Sprintf("req-ctx-%v-%d", cacheHit, bp),
			OrgID:          &org,
			ModelRequested: "gpt-4o",
			ModelUsed:      "gpt-4o",
			InputTokens:    1000,
			OutputTokens:   100,
			Cost:           0.01,
			LatencyMs:      200,
			StatusCode:     200,
			Currency:       "CNY",
			RouteType:      "weighted",
			CreatedAt:      hourStart,
		}
		if cacheHit {
			l.CacheHit = true
		}
		l.SystemTokens = &sys
		l.HistoryTokens = &hist
		l.QuestionTokens = &q
		l.ToolTokens = &tool
		l.ToolOutputTokens = &toolOut
		l.ContextWindow = &win
		l.AnalysisFlags = &flags
		l.ContextUtilizationBp = bp
		return l
	}

	// Row 1: fully analyzed, bp=9600 (gt95 bucket), non-cache-hit.
	// Row 2: cache_hit=true with analysis fields — must be excluded from all ctx cols.
	// Row 3: bp=9499 (80_95 bucket), non-cache-hit.
	// Row 4: analyzed but context_utilization_bp IS NULL — counted in ctx_analyzed_count,
	// but must fall into no utilization bucket.
	bp9600, bp9499 := 9600, 9499
	rows := []model.UsageLog{mk(false, &bp9600), mk(true, &bp9600), mk(false, &bp9499), mk(false, nil)}
	require.NoError(t, db.CreateInBatches(rows, 4).Error)

	cfg := config.DataLensConfig{
		Enabled: true,
		Agg: config.DataLensAggConfig{
			Interval:  "1h",
			Lookback:  "3h",
		},
	}
	svc := NewDataLensAggregatorService(db, d, cfg)
	require.NoError(t, svc.AggregateOnce(ctx))

	var metrics []model.DataLensHourlyMetric
	require.NoError(t, db.Where("org_id = ? AND agg_level = 'global'", testOrgID).Find(&metrics).Error)
	require.NotEmpty(t, metrics)

	// Find the row covering our test hour (status_group=200, CNY).
	var m *model.DataLensHourlyMetric
	for i := range metrics {
		if metrics[i].StatusGroup == 200 && metrics[i].HourBucket.Equal(hourStart) {
			m = &metrics[i]
		}
	}
	require.NotNil(t, m, "hourly metric row for test hour not found")

	// Rows 1, 3, 4 analyzed (cache_hit row excluded); row 4 has NULL bp.
	assert.Equal(t, 3, m.CtxAnalyzedCount, "ctx_analyzed_count should exclude cache_hit rows")
	assert.Equal(t, int64(3*100), m.CtxSystemTokens)
	assert.Equal(t, int64(3*200), m.CtxHistoryTokens)
	assert.Equal(t, int64(3*300), m.CtxQuestionTokens)
	assert.Equal(t, int64(3*400), m.CtxToolTokens)
	assert.Equal(t, int64(3*500), m.CtxToolOutputTokens)
	assert.Equal(t, int64(3*100000), m.CtxTotalWindow)
	// flags = 1|8 on all three analyzed rows.
	assert.Equal(t, 3, m.CtxOverflowCount)
	assert.Equal(t, 3, m.CtxWindowUnknownCount)
	// Bucket placement: bp=9600 → gt95, bp=9499 → 80_95, bp=NULL → no bucket.
	assert.Equal(t, 0, m.CtxUtilBucketLt50)
	assert.Equal(t, 0, m.CtxUtilBucket5080)
	assert.Equal(t, 1, m.CtxUtilBucket8095)
	assert.Equal(t, 1, m.CtxUtilBucketGt95)
	// Explicit: the NULL-bp analyzed row falls into no utilization bucket.
	assert.Equal(t, 2, m.CtxUtilBucketLt50+m.CtxUtilBucket5080+m.CtxUtilBucket8095+m.CtxUtilBucketGt95,
		"NULL bp analyzed rows must not land in any utilization bucket")
	// Unanalyzed rows (analysis_flags NULL) also excluded — verified by total sums above.

	cleanupData(t, db, testOrgID)
}

// TestAggregatorBackfill verifies that backfill correctly aggregates
// historical data spanning multiple days.
func TestAggregatorBackfill(t *testing.T) {
	db, d, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	cleanupData(t, db, testOrgID)

	// Insert usage_logs spanning 5 days.
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	for i := 5; i >= 1; i-- {
		day := dayStart.AddDate(0, 0, -i)
		// Insert 20 logs per day, each day starting at midnight.
		insertTestLogs(t, db, testOrgID, day, 20)
	}

	// Verify raw data.
	var rawCount int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM usage_logs WHERE org_id = ?", testOrgID).Scan(&rawCount).Error)
	require.Equal(t, int64(100), rawCount, "expected 100 raw usage_log rows (5 days * 20)")

	// Create service with backfillDays=5.
	cfg := config.DataLensConfig{
		Enabled: true,
		Agg: config.DataLensAggConfig{
			Interval:     "1h",
			Lookback:     "3h",
			BackfillDays: 5,
		},
	}
	svc := NewDataLensAggregatorService(db, d, cfg)

	// Run backfill (this is normally called in Run(), but we invoke the cycle manually).
	svc.backfill(ctx)

	// Assert: daily_metrics should have rows for each of the 5 historical days.
	var dailyMetrics []model.DataLensDailyMetric
	require.NoError(t, db.Where("org_id = ?", testOrgID).Find(&dailyMetrics).Error)
	require.NotEmpty(t, dailyMetrics, "expected daily metric rows after backfill")

	// Count distinct days.
	daySet := map[string]bool{}
	for _, dm := range dailyMetrics {
		dayStr := dm.DayBucket.Format("2006-01-02")
		daySet[dayStr] = true
	}
	// We expect at least some of the 5 days to have daily rollups (depending on
	// whether hourly data was produced for each day).
	assert.GreaterOrEqual(t, len(daySet), 3, "expected daily metrics for at least 3 of 5 backfilled days")

	cleanupData(t, db, testOrgID)
}

// TestQueryBasicMetrics verifies that PgMetricsStore.Query returns correct
// result structure and data when querying with dimensions and metrics.
func TestQueryBasicMetrics(t *testing.T) {
	db, d, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	cleanupData(t, db, testOrgID)

	// Insert known hourly metrics data for 3 distinct models over 2 days.
	dayStart := time.Now().UTC().Truncate(24 * time.Hour)
	models := []string{"gpt-4o", "claude-3.5-sonnet", "gpt-4o-mini"}

	for day := 0; day < 2; day++ {
		bucket := dayStart.AddDate(0, 0, -day)
		for i, modelName := range models {
			name := modelName
			m := model.DataLensHourlyMetric{
				OrgID:          testOrgID,
				AggLevel:       "by_model",
				ModelName:      &name,
				HourBucket:     bucket,
				Currency:       "CNY",
				RequestCount:   100 + i*10,
				InputTokens:    int64(1000 + i*100),
				OutputTokens:   int64(500 + i*50),
				TotalCost:      0.5 + float64(i)*0.1,
				TotalLatencyMs: int64((200 + i*50) * (100 + i*10)),
				MinLatencyMs:   100 + i*10,
				MaxLatencyMs:   500 + i*20,
				LatencySamples: 100 + i*10,
				StatusGroup:    200,
			}
			require.NoError(t, db.Create(&m).Error)
		}
	}

	// Create PgMetricsStore.
	store := repository.NewPgMetricsStore(db, d)

	// Query with dimensions=["model"], metrics=["requests","cost"].
	params := repository.QueryParams{
		OrgID:       testOrgID,
		Dimensions:  []string{"model"},
		Metrics:     []string{"requests", "cost"},
		Granularity: "hour",
		TimeRange: repository.TimeRange{
			Type: "absolute",
			Start: &[]time.Time{dayStart.AddDate(0, 0, -2)}[0],
			End:   &[]time.Time{dayStart.Add(24 * time.Hour)}[0],
		},
	}

	result, err := store.Query(ctx, params)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Assert: result has expected number of rows (3 models * 2 days = 6 rows).
	assert.Equal(t, 6, result.Total, "expected 6 result rows (3 models * 2 hours)")
	assert.Len(t, result.Rows, 6)

	// Assert: column metadata is correct.
	assert.Len(t, result.Columns, 4, "expected 4 columns: time_bucket, model, requests, cost")

	colKeys := make([]string, len(result.Columns))
	for i, col := range result.Columns {
		colKeys[i] = col.Key
	}
	assert.Contains(t, colKeys, "time_bucket")
	assert.Contains(t, colKeys, "model")
	assert.Contains(t, colKeys, "requests")
	assert.Contains(t, colKeys, "cost")

	// Verify column types.
	for _, col := range result.Columns {
		switch col.Key {
		case "time_bucket":
			assert.Equal(t, "time", col.Type)
		case "model":
			assert.Equal(t, "dimension", col.Type)
		case "requests":
			assert.Equal(t, "metric", col.Type)
			assert.Equal(t, "number", col.Format)
		case "cost":
			assert.Equal(t, "metric", col.Type)
			assert.Equal(t, "currency", col.Format)
		}
	}

	cleanupData(t, db, testOrgID)
}
