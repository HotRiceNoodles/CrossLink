-- M3b 工具维度日聚合表补齐 sqlite 链（pg 000089+000091 合并建表，含 disappeared_count）。
-- 与 mysql 000016 同理：表跨方言存在，聚合 SQL 的 PG-only 是 U-11 的已知限制。
-- SQLite 支持表达式索引，唯一 upsert 目标与 PG 相同直接 COALESCE。
CREATE TABLE datalens_tool_daily_metrics (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id                   INTEGER NOT NULL,
    day_bucket               TEXT NOT NULL,
    api_key_id               INTEGER,
    tool_name                TEXT NOT NULL,
    mcp_calls                INTEGER NOT NULL DEFAULT 0,
    mcp_errors               INTEGER NOT NULL DEFAULT 0,
    mcp_blocked              INTEGER NOT NULL DEFAULT 0,
    total_duration_ms        INTEGER NOT NULL DEFAULT 0,
    duration_samples         INTEGER NOT NULL DEFAULT 0,
    first_seen_count         INTEGER NOT NULL DEFAULT 0,
    definition_changed_count INTEGER NOT NULL DEFAULT 0,
    disappeared_count        INTEGER NOT NULL DEFAULT 0,
    created_at               DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_dtdm_upsert ON datalens_tool_daily_metrics (org_id, COALESCE(api_key_id, -1), day_bucket, tool_name);
CREATE INDEX idx_dtdm_org_day ON datalens_tool_daily_metrics (org_id, day_bucket);
CREATE INDEX idx_dtdm_tool_day ON datalens_tool_daily_metrics (org_id, tool_name, day_bucket);
