package repository

import (
	"context"
	"testing"
	"time"

	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newToolMetricsDB(t *testing.T) (*PgMetricsStore, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.DataLensToolDailyMetric{}, &model.APIKey{}))
	return NewPgMetricsStore(db, dialect.NewSQLiteDialect(dialect.DBConfig{})), db
}

func seedToolRows(t *testing.T, db *gorm.DB) {
	t.Helper()
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	key7 := int64(7)
	key8 := int64(8)
	rows := []model.DataLensToolDailyMetric{
		// exec_shell: LLM-side tool, events only (no mcp_* counters).
		{OrgID: 1, DayBucket: day, APIKeyID: &key7, ToolName: "exec_shell", FirstSeenCount: 1, DefinitionChangedCount: 2, DisappearedCount: 1},
		// mcp tool, same key/day: calls + errors + duration.
		{OrgID: 1, DayBucket: day, APIKeyID: &key7, ToolName: "mcp/fs/read_file", McpCalls: 10, McpErrors: 2, McpBlocked: 1, TotalDurationMs: 1000, DurationSamples: 8},
		// Same tool next day — must not merge into the day-20 row.
		{OrgID: 1, DayBucket: day.AddDate(0, 0, 1), APIKeyID: &key7, ToolName: "mcp/fs/read_file", McpCalls: 5, TotalDurationMs: 400, DurationSamples: 5},
		// Other key, other org (row must be invisible to org 1 queries).
		{OrgID: 2, DayBucket: day, APIKeyID: &key8, ToolName: "exec_shell", McpCalls: 99, FirstSeenCount: 9},
	}
	for i := range rows {
		require.NoError(t, db.Create(&rows[i]).Error)
	}
	require.NoError(t, db.Create(&model.APIKey{ID: 7, Name: "smoke-key", KeyHash: "h7", KeyPrefix: "cl-x", Status: 1}).Error)
}

func toolTimeRange() TimeRange {
	// Single-day window (day 20) so one row per group — read_file also has a
	// day-21 row that must stay out of these assertions.
	return TimeRange{Type: "absolute",
		Start: ptrTime(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)),
		End:   ptrTime(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestQueryToolMetrics_GroupingAndAggregates(t *testing.T) {
	s, db := newToolMetricsDB(t)
	seedToolRows(t, db)

	res, err := s.Query(context.Background(), QueryParams{
		OrgID:       1,
		Dimensions:  []string{"tool"},
		Metrics:     []string{"mcp_calls", "mcp_avg_duration", "tool_first_seen", "tool_changed", "tool_disappeared"},
		TimeRange:   toolTimeRange(),
		Granularity: "day",
		SortBy:      "mcp_calls",
		SortOrder:   "desc",
	})
	require.NoError(t, err)
	assert.Equal(t, "datalens_tool_daily_metrics", res.Meta.DataSource)

	byTool := map[string]map[string]any{}
	for _, r := range res.Rows {
		byTool[r["tool_name"].(string)] = r
	}
	// exec_shell spans two days (day 20 + day 21 rows exist only on day 20 for
	// exec_shell; read_file spans both days).
	require.Contains(t, byTool, "exec_shell")
	require.Contains(t, byTool, "mcp/fs/read_file")

	// Day 20 row: read_file calls=10, avg duration = 1000/8 = 125ms.
	rf := byTool["mcp/fs/read_file"]
	assert.EqualValues(t, 10, rf["mcp_calls"])
	assert.EqualValues(t, 125, rf["mcp_avg_duration"])
	// exec_shell: events only — first_seen=1, changed=2, no mcp counters.
	es := byTool["exec_shell"]
	assert.EqualValues(t, 1, es["tool_first_seen"])
	assert.EqualValues(t, 2, es["tool_changed"])
	assert.EqualValues(t, 1, es["tool_disappeared"])
	assert.EqualValues(t, 0, es["mcp_calls"])
}

func TestQueryToolMetrics_KeyDimensionResolvesNames(t *testing.T) {
	s, db := newToolMetricsDB(t)
	seedToolRows(t, db)

	res, err := s.Query(context.Background(), QueryParams{
		OrgID:       1,
		Dimensions:  []string{"key"},
		Metrics:     []string{"tool_first_seen"},
		TimeRange:   toolTimeRange(),
		Granularity: "day",
	})
	require.NoError(t, err)
	// Org 1 has key 7 only (key 8 belongs to org 2); name resolved from api_keys.
	require.Len(t, res.Rows, 1)
	assert.Equal(t, "smoke-key", res.Rows[0]["key"])
	assert.EqualValues(t, 1, res.Rows[0]["tool_first_seen"])
}

func TestQueryToolMetrics_ToolFilter(t *testing.T) {
	s, db := newToolMetricsDB(t)
	seedToolRows(t, db)

	res, err := s.Query(context.Background(), QueryParams{
		OrgID:       1,
		Dimensions:  []string{"tool"},
		Metrics:     []string{"mcp_calls"},
		Filters:     []Filter{{Dimension: "tool", Operator: "eq", Value: "mcp/fs/read_file"}},
		TimeRange:   toolTimeRange(),
		Granularity: "day",
	})
	require.NoError(t, err)
	require.Len(t, res.Rows, 1) // single-day window
	for _, r := range res.Rows {
		assert.Equal(t, "mcp/fs/read_file", r["tool_name"])
		assert.EqualValues(t, 10, r["mcp_calls"])
	}
}

func TestQueryToolMetrics_RejectsMixing(t *testing.T) {
	s, db := newToolMetricsDB(t)
	seedToolRows(t, db)
	ctx := context.Background()

	// Usage metric with tool dimension.
	_, err := s.Query(ctx, QueryParams{
		OrgID: 1, Dimensions: []string{"tool"}, Metrics: []string{"requests"},
		TimeRange: toolTimeRange(), Granularity: "day",
	})
	require.ErrorContains(t, err, "not available for the tool dimension")

	// Tool dimension combined with usage-table dimension.
	_, err = s.Query(ctx, QueryParams{
		OrgID: 1, Dimensions: []string{"tool", "model"}, Metrics: []string{"mcp_calls"},
		TimeRange: toolTimeRange(), Granularity: "day",
	})
	require.ErrorContains(t, err, "cannot be combined with tool metrics")

	// Hourly granularity on a daily rollup.
	_, err = s.Query(ctx, QueryParams{
		OrgID: 1, Dimensions: []string{"tool"}, Metrics: []string{"mcp_calls"},
		TimeRange: toolTimeRange(), Granularity: "hour",
	})
	require.ErrorContains(t, err, "daily-granularity only")

	// Tool metric without tool dimension still routes to the tool table —
	// dimensions default to time only, which is legal.
	res, err := s.Query(ctx, QueryParams{
		OrgID: 1, Dimensions: []string{}, Metrics: []string{"mcp_calls"},
		TimeRange: toolTimeRange(), Granularity: "day",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Rows)
}
