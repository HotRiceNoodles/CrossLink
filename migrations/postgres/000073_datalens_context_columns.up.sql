-- DataLens context-analysis pre-aggregated columns (hourly + daily)
ALTER TABLE datalens_hourly_metrics
    ADD COLUMN ctx_system_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_history_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_question_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_output_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_total_window BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_analyzed_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_overflow_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_window_unknown_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_lt50 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_50_80 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_80_95 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_gt95 INT NOT NULL DEFAULT 0;

ALTER TABLE datalens_daily_metrics
    ADD COLUMN ctx_system_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_history_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_question_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_tool_output_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_total_window BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_analyzed_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_overflow_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_window_unknown_count INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_lt50 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_50_80 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_80_95 INT NOT NULL DEFAULT 0,
    ADD COLUMN ctx_util_bucket_gt95 INT NOT NULL DEFAULT 0;
