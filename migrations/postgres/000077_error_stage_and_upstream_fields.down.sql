ALTER TABLE usage_logs
    DROP COLUMN upstream_error_type,
    DROP COLUMN upstream_error_param,
    DROP COLUMN error_stage;
