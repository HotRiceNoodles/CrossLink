-- Error observability L2 (docs/plans/2026-10-07-error-observability-design.md):
-- per-provider attempt timeline for fallback requests, so the log detail can
-- show which routes were tried, which failed and why fallback stopped.
-- NULL on clean single-attempt successes to keep row size flat.
ALTER TABLE usage_logs ADD COLUMN attempts jsonb;
