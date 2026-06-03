package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPRepo_GetToolCallStats_SQLite(t *testing.T) {
	repo := NewMCPRepo(testDB, testDialect)

	now := time.Now()
	// Insert tool call logs with different statuses
	logs := []*MCPToolCallLog{
		{RequestID: "r1", ServerName: "s1", ToolName: "search", Method: "tools/call", InputSize: 100, OutputSize: 200, Duration: 50, Status: 1, CreatedAt: now},
		{RequestID: "r2", ServerName: "s1", ToolName: "search", Method: "tools/call", InputSize: 80, OutputSize: 150, Duration: 30, Status: 1, CreatedAt: now},
		{RequestID: "r3", ServerName: "s1", ToolName: "read", Method: "tools/call", InputSize: 50, OutputSize: 100, Duration: 20, Status: 0, CreatedAt: now},
		{RequestID: "r4", ServerName: "s1", ToolName: "delete", Method: "tools/call", InputSize: 10, OutputSize: 0, Duration: 5, Status: -1, CreatedAt: now},
	}
	for _, l := range logs {
		require.NoError(t, repo.LogToolCall(testCtx, l))
	}

	stats, err := repo.GetToolCallStats(testCtx, 0, 0, 30)
	require.NoError(t, err)

	assert.Equal(t, int64(4), stats.TotalCalls, "TotalCalls")
	assert.Equal(t, int64(2), stats.SuccessCount, "SuccessCount (status=1)")
	assert.Equal(t, int64(1), stats.ErrorCount, "ErrorCount (status=0)")
	assert.Equal(t, int64(1), stats.BlockedCount, "BlockedCount (status=-1)")
	assert.Equal(t, int64(240), stats.TotalInput, "TotalInput (100+80+50+10)")
	assert.Equal(t, int64(450), stats.TotalOutput, "TotalOutput (200+150+100+0)")
	// P95 is 0 on SQLite (PG-only feature)
	assert.Equal(t, float64(0), stats.P95Duration, "P95Duration should be 0 on non-PG")
}

func TestMCPRepo_GetTopTools_SQLite(t *testing.T) {
	repo := NewMCPRepo(testDB, testDialect)

	now := time.Now()
	// search: 3 success, 1 error
	for i := 0; i < 3; i++ {
		require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{
			RequestID: "s-success", ServerName: "s1", ToolName: "search", Method: "tools/call",
			Duration: 50, Status: 1, CreatedAt: now,
		}))
	}
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{
		RequestID: "s-error", ServerName: "s1", ToolName: "search", Method: "tools/call",
		Duration: 100, Status: 0, CreatedAt: now,
	}))

	// read: 1 success
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{
		RequestID: "r1", ServerName: "s1", ToolName: "read", Method: "tools/call",
		Duration: 20, Status: 1, CreatedAt: now,
	}))

	tools, err := repo.GetTopTools(testCtx, 0, 0, 30, 10)
	require.NoError(t, err)

	require.Len(t, tools, 2, "should return 2 tools")

	// First tool should be "search" (4 calls)
	assert.Equal(t, "search", tools[0].Name)
	assert.Equal(t, int64(4), tools[0].Count)
	// error_rate = (1 error + 0 blocked) / 4 total = 0.25
	assert.InDelta(t, 0.25, tools[0].ErrorRate, 0.01, "search error_rate should be ~0.25")

	// Second tool should be "read" (1 call)
	assert.Equal(t, "read", tools[1].Name)
	assert.Equal(t, int64(1), tools[1].Count)
	assert.InDelta(t, 0.0, tools[1].ErrorRate, 0.01, "read error_rate should be 0")
}

func TestMCPRepo_GetCallsByDay_SQLite(t *testing.T) {
	repo := NewMCPRepo(testDB, testDialect)

	now := time.Now()
	day1 := time.Date(now.Year(), now.Month(), now.Day()-2, 10, 0, 0, 0, now.Location())
	day2 := time.Date(now.Year(), now.Month(), now.Day()-1, 14, 0, 0, 0, now.Location())
	day3 := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, now.Location())

	// Day 1: 2 success, 1 error
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{RequestID: "d1a", ServerName: "s1", ToolName: "t", Method: "tools/call", Status: 1, CreatedAt: day1}))
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{RequestID: "d1b", ServerName: "s1", ToolName: "t", Method: "tools/call", Status: 1, CreatedAt: day1}))
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{RequestID: "d1c", ServerName: "s1", ToolName: "t", Method: "tools/call", Status: 0, CreatedAt: day1}))

	// Day 2: 1 blocked
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{RequestID: "d2a", ServerName: "s1", ToolName: "t", Method: "tools/call", Status: -1, CreatedAt: day2}))

	// Day 3: 1 success
	require.NoError(t, repo.LogToolCall(testCtx, &MCPToolCallLog{RequestID: "d3a", ServerName: "s1", ToolName: "t", Method: "tools/call", Status: 1, CreatedAt: day3}))

	result, err := repo.GetCallsByDay(testCtx, 0, 0, 30)
	require.NoError(t, err)

	require.Len(t, result, 3, "should return 3 daily buckets")

	// Day 1: 2 success, 1 error
	assert.Equal(t, int64(3), result[0].Count)
	assert.Equal(t, int64(2), result[0].Success)
	assert.Equal(t, int64(1), result[0].Error)

	// Day 2: 0 success, 1 error (blocked counts as error)
	assert.Equal(t, int64(1), result[1].Count)
	assert.Equal(t, int64(0), result[1].Success)
	assert.Equal(t, int64(1), result[1].Error)

	// Day 3: 1 success, 0 error
	assert.Equal(t, int64(1), result[2].Count)
	assert.Equal(t, int64(1), result[2].Success)
	assert.Equal(t, int64(0), result[2].Error)
}
