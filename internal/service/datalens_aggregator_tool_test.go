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

// TestBuildToolDailySQL asserts the shape of the tool rollup INSERT without
// needing a database. Semantic behavior (two sources folded into one insert,
// mcp name prefixing, key-0 → NULL) is covered here; data correctness lives
// in the integration test.
func TestBuildToolDailySQL(t *testing.T) {
	d := dialect.NewPostgresDialect(dialect.DBConfig{})
	svc := NewDataLensAggregatorService(nil, d, config.DataLensConfig{})

	start := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	sql, args := svc.buildToolDailySQL(start, end)

	// Each source's WHERE binds its own positional placeholders.
	require.Equal(t, []any{start, end, start, end}, args)

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
	} {
		assert.Contains(t, sql, want)
	}
	// No PG-only constructs: the SQL must run on mysql/sqlite as-is.
	assert.NotContains(t, sql, "ON CONFLICT")
	assert.NotContains(t, sql, "$1")
	assert.Equal(t, 2, strings.Count(sql, "created_at >= ? AND"))
	assert.Equal(t, 2, strings.Count(sql, "created_at < ? AND"))
}

// TestBuildToolDailySQL_MySQLShape: the MySQL family needs CONCAT() (|| is
// logical OR there).
func TestBuildToolDailySQL_MySQLShape(t *testing.T) {
	for _, name := range []string{"mysql", "oceanbase"} {
		var d dialect.Dialect
		switch name {
		case "mysql":
			d = dialect.NewMySQLDialect(dialect.DBConfig{})
		case "oceanbase":
			d = dialect.NewOceanBaseDialect(dialect.DBConfig{})
		}
		svc := NewDataLensAggregatorService(nil, d, config.DataLensConfig{})
		sql, _ := svc.buildToolDailySQL(time.Now(), time.Now().Add(time.Hour))
		assert.Contains(t, sql, "CONVERT(CONCAT('mcp/', l.server_name, '/', l.tool_name) USING utf8mb4) AS tool_name", name)
		assert.Contains(t, sql, "CONVERT(e.tool_name USING utf8mb4) AS tool_name", name)
		assert.NotContains(t, sql, "'mcp/' ||", name)
	}
}
