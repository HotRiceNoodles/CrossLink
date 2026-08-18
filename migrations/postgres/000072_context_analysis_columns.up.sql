ALTER TABLE usage_logs
    ADD COLUMN system_tokens INT DEFAULT NULL,
    ADD COLUMN history_tokens INT DEFAULT NULL,
    ADD COLUMN question_tokens INT DEFAULT NULL,
    ADD COLUMN tool_tokens INT DEFAULT NULL,
    ADD COLUMN tool_output_tokens INT DEFAULT NULL,
    ADD COLUMN context_window INT DEFAULT NULL,
    ADD COLUMN context_utilization_bp INT DEFAULT NULL,
    ADD COLUMN analysis_flags INT DEFAULT NULL,
    ADD COLUMN context_snapshot JSONB DEFAULT NULL;
