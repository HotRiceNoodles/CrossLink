-- Error observability L1.5 (docs/plans/2026-10-07-error-observability-design.md):
-- upstream error.type/param (OpenAI param names the rejected request field —
-- the most direct 400 locator) and the gateway stage the request died at,
-- so a 400 row says both WHICH field was rejected and WHERE it failed.
ALTER TABLE usage_logs
    ADD COLUMN upstream_error_type varchar(64),
    ADD COLUMN upstream_error_param varchar(128),
    ADD COLUMN error_stage varchar(16);
