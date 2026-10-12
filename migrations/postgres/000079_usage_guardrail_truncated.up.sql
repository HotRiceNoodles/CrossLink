-- Partial-coverage observability (round 10): mark usage rows whose guardrail
-- check only inspected the content prefix (>500K runes, oversize=truncate).
ALTER TABLE usage_logs ADD COLUMN guardrail_truncated BOOLEAN NOT NULL DEFAULT false;
