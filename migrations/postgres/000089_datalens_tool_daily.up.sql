-- Agent toolset daily rollup for DataLens tool dimension (M3b).
-- Sources: mcp_tool_call_logs (per-call) + agent_toolset_events (change history).
-- PostgreSQL-only chain, matching the other datalens pre-agg tables (U-11:
-- aggregation SQL uses $N placeholders and expression ON CONFLICT targets).
CREATE TABLE datalens_tool_daily_metrics (
    id BIGSERIAL PRIMARY KEY,
    org_id BIGINT NOT NULL,
    day_bucket DATE NOT NULL,
    api_key_id BIGINT NULL,
    tool_name VARCHAR(256) NOT NULL,
    mcp_calls BIGINT NOT NULL DEFAULT 0,
    mcp_errors BIGINT NOT NULL DEFAULT 0,
    mcp_blocked BIGINT NOT NULL DEFAULT 0,
    total_duration_ms BIGINT NOT NULL DEFAULT 0,
    duration_samples BIGINT NOT NULL DEFAULT 0,
    first_seen_count INT NOT NULL DEFAULT 0,
    definition_changed_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Upsert target must match the ON CONFLICT expression in the aggregator.
CREATE UNIQUE INDEX idx_dtdm_upsert ON datalens_tool_daily_metrics (
    org_id, COALESCE(api_key_id, -1), day_bucket, tool_name
);
CREATE INDEX idx_dtdm_org_day ON datalens_tool_daily_metrics (org_id, day_bucket);
CREATE INDEX idx_dtdm_tool_day ON datalens_tool_daily_metrics (org_id, tool_name, day_bucket);
