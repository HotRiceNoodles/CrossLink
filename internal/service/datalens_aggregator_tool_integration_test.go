//go:build integration

package service

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestAggregatorToolDailyRollup verifies the M3b tool rollup: mcp_tool_call_logs
// and agent_toolset_events fold into datalens_tool_daily_metrics, with mcp/
// name prefixing, key-0 normalization to NULL, and idempotent re-runs.
func TestAggregatorToolDailyRollup(t *testing.T) {
	db, d, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	// Seed inside a COMPLETE day: the rollup window is [today-3d 00:00, today
	// 00:00) — today's partial day is excluded, same semantics as daily_metrics.
	seedTime := time.Now().UTC().AddDate(0, 0, -1)

	// Clean stale test data (also covers the rollup + both source tables).
	db.Exec("DELETE FROM datalens_tool_daily_metrics WHERE org_id = ?", testOrgID)
	db.Exec("DELETE FROM mcp_tool_call_logs WHERE org_id = ?", testOrgID)
	db.Exec("DELETE FROM agent_toolset_events WHERE org_id = ?", testOrgID)
	defer func() {
		db.Exec("DELETE FROM datalens_tool_daily_metrics WHERE org_id = ?", testOrgID)
		db.Exec("DELETE FROM mcp_tool_call_logs WHERE org_id = ?", testOrgID)
		db.Exec("DELETE FROM agent_toolset_events WHERE org_id = ?", testOrgID)
	}()

	insertMCPLogs(t, db, testOrgID, seedTime)

	// Toolset events: same key, two tools; one first_seen + one changed + one
	// disappeared (disappeared rollup column, pg migration 000091).
	evs := []model.AgentToolsetEvent{
		{OrgID: testOrgID, APIKeyID: 42, ToolName: "exec_shell", EventType: "first_seen", CreatedAt: seedTime},
		{OrgID: testOrgID, APIKeyID: 42, ToolName: "mcp/fs/read_file", EventType: "definition_changed", CreatedAt: seedTime},
		{OrgID: testOrgID, APIKeyID: 42, ToolName: "mcp/fs/read_file", EventType: "disappeared", CreatedAt: seedTime},
	}
	for i := range evs {
		require.NoError(t, db.Create(&evs[i]).Error)
	}

	cfg := config.DataLensConfig{Enabled: true, Agg: config.DataLensAggConfig{Interval: "1h", Lookback: "3h"}}
	svc := NewDataLensAggregatorService(db, d, cfg)
	require.NoError(t, svc.AggregateOnce(ctx))

	type row struct {
		ToolName               string
		APIKeyID               *int64
		McpCalls               int64
		McpErrors              int64
		McpBlocked             int64
		TotalDurationMs        int64
		DurationSamples        int64
		FirstSeenCount         int
		DefinitionChangedCount int
		DisappearedCount       int
	}
	var rows []row
	require.NoError(t, db.Raw(`
		SELECT tool_name, api_key_id, mcp_calls, mcp_errors, mcp_blocked,
		       total_duration_ms, duration_samples, first_seen_count, definition_changed_count, disappeared_count
		FROM datalens_tool_daily_metrics WHERE org_id = ? ORDER BY tool_name`, testOrgID).Scan(&rows).Error)

	byTool := map[string]row{}
	for _, r := range rows {
		byTool[r.ToolName] = r
	}
	require.Len(t, byTool, 3, "expected three distinct tools, got %+v", rows)

	// exec_shell: events only.
	es := byTool["exec_shell"]
	assert.Equal(t, 1, es.FirstSeenCount)
	assert.Equal(t, int64(0), es.McpCalls)

	// mcp/fs/read_file: 3 calls (1 success 120ms, 1 error, 1 blocked) + 1
	// definition_changed from the events source on the SAME row.
	rf := byTool["mcp/fs/read_file"]
	assert.Equal(t, int64(3), rf.McpCalls)
	assert.Equal(t, int64(1), rf.McpErrors)
	assert.Equal(t, int64(1), rf.McpBlocked)
	assert.Equal(t, int64(120), rf.TotalDurationMs)
	assert.Equal(t, int64(1), rf.DurationSamples)
	assert.Equal(t, 1, rf.DefinitionChangedCount)
	assert.Equal(t, 1, rf.DisappearedCount, "disappeared event must roll up (000091 column)")
	require.NotNil(t, rf.APIKeyID)
	assert.EqualValues(t, 42, *rf.APIKeyID)

	// mcp/fs/write_file: key-0 (config-auth) normalizes to NULL.
	wf := byTool["mcp/fs/write_file"]
	assert.Equal(t, int64(2), wf.McpCalls)
	assert.Nil(t, wf.APIKeyID)

	// Idempotency: re-run the cycle, counters must not move.
	require.NoError(t, svc.AggregateOnce(ctx))
	var rows2 []row
	require.NoError(t, db.Raw(`
		SELECT tool_name, mcp_calls, first_seen_count, definition_changed_count
		FROM datalens_tool_daily_metrics WHERE org_id = ? ORDER BY tool_name`, testOrgID).Scan(&rows2).Error)
	require.Len(t, rows2, 3)
	for _, r := range rows2 {
		switch r.ToolName {
		case "exec_shell":
			assert.Equal(t, int64(0), r.McpCalls)
			assert.Equal(t, 1, r.FirstSeenCount)
		case "mcp/fs/read_file":
			assert.Equal(t, int64(3), r.McpCalls)
			assert.Equal(t, 1, r.DefinitionChangedCount)
		case "mcp/fs/write_file":
			assert.Equal(t, int64(2), r.McpCalls)
		}
	}
}

// insertMCPLogs seeds mcp_tool_call_logs (partitioned table, raw SQL) at the
// given timestamp: read_file = 1 success + 1 error + 1 blocked, write_file =
// 2 success with api_key_id = 0 (config-auth), plus one tools/list row that
// must NOT count as a tool call.
func insertMCPLogs(t *testing.T, db *gorm.DB, orgID int64, at time.Time) {
	t.Helper()
	org := int64(orgID)
	type log struct {
		org    *int64
		key    int64
		server string
		tool   string
		method string
		status int16
		dur    int
	}
	logs := []log{
		{&org, 42, "fs", "read_file", "tools/call", 1, 120},
		{&org, 42, "fs", "read_file", "tools/call", 0, 30},
		{&org, 42, "fs", "read_file", "tools/call", -1, 5},
		{&org, 0, "fs", "write_file", "tools/call", 1, 50},
		{&org, 0, "fs", "write_file", "tools/call", 1, 60},
		{&org, 42, "fs", "read_file", "tools/list", 1, 10}, // discovery, not a call
	}
	for _, l := range logs {
		require.NoError(t, db.Exec(`
			INSERT INTO mcp_tool_call_logs (org_id, request_id, server_id, server_name, tool_name, method,
			                                input_size, output_size, duration, status, api_key_id, team_id, created_at)
			VALUES (?, 'r', 1, ?, ?, ?, 0, 0, ?, ?, ?, 0, ?)`,
			l.org, l.server, l.tool, l.method, l.dur, l.status, l.key, at).Error)
	}
}
