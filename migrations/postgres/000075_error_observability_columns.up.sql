-- Error observability (docs/plans/2026-10-07-error-observability-design.md L1):
-- persist the sanitized upstream error message and the upstream status/code so
-- 4xx/5xx rows are diagnosable from the log detail view.
-- upstream_status NULL distinguishes gateway-side rejections (invalid request,
-- guardrail) from upstream rejections.
ALTER TABLE usage_logs
    ADD COLUMN error_message text,
    ADD COLUMN upstream_status int,
    ADD COLUMN upstream_error_code varchar(64);
