-- Fix: mcp_tool_call_logs was created as PARTITION BY RANGE with zero partitions.
-- All INSERTs fail with "no partition of relation found".
-- Create initial monthly partitions for 2026 and a default partition.

-- Create monthly partitions for 2026
CREATE TABLE mcp_tool_call_logs_2026_01 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE mcp_tool_call_logs_2026_02 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE mcp_tool_call_logs_2026_03 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
CREATE TABLE mcp_tool_call_logs_2026_04 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');
CREATE TABLE mcp_tool_call_logs_2026_05 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE mcp_tool_call_logs_2026_06 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE mcp_tool_call_logs_2026_07 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');
CREATE TABLE mcp_tool_call_logs_2026_08 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE mcp_tool_call_logs_2026_09 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE mcp_tool_call_logs_2026_10 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE mcp_tool_call_logs_2026_11 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
CREATE TABLE mcp_tool_call_logs_2026_12 PARTITION OF mcp_tool_call_logs
    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');

-- Default partition catches rows outside the explicit range.
CREATE TABLE mcp_tool_call_logs_default PARTITION OF mcp_tool_call_logs DEFAULT;
