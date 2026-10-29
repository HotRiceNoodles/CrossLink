package service

import (
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildToolDailySQL asserts the shape of the tool rollup UPSERT without
// needing a database. Semantic behavior (two sources folded into one upsert,
// mcp name prefixing, key-0 → NULL, expression conflict target) is covered
// here; data correctness lives in the integration test.
func TestBuildToolDailySQL(t *testing.T) {
	d := dialect.NewPostgresDialect(dialect.DBConfig{})
	svc := NewDataLensAggregatorService(nil, d, config.DataLensConfig{})

	start := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	sql, args := svc.buildToolDailySQL(start, end)

	require.Equal(t, []any{start, end}, args)

	for _, want := range []string{
		"INSERT INTO datalens_tool_daily_metrics",
		"FROM mcp_tool_call_logs l",
		"l.method = 'tools/call'",
		"'mcp/' || l.server_name || '/' || l.tool_name AS tool_name",
		"NULLIF(l.api_key_id, 0) AS api_key_id",
		"UNION ALL",
		"FROM agent_toolset_events e",
		"e.event_type = 'first_seen'",
		"e.event_type = 'definition_changed'",
		"e.event_type = 'disappeared'",
		"ON CONFLICT (org_id, COALESCE(api_key_id, -1), day_bucket, tool_name)",
		"first_seen_count = EXCLUDED.first_seen_count",
		"disappeared_count = EXCLUDED.disappeared_count",
		"mcp_calls = EXCLUDED.mcp_calls",
	} {
		assert.Contains(t, sql, want)
	}
	// Both branches must reference the same time window placeholders.
	assert.Equal(t, 2, strings.Count(sql, "created_at >= $1 AND"))
	assert.Equal(t, 2, strings.Count(sql, "created_at < $2 AND"))
}
