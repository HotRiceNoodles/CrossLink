ALTER TABLE usage_logs
    DROP COLUMN error_message,
    DROP COLUMN upstream_status,
    DROP COLUMN upstream_error_code;
