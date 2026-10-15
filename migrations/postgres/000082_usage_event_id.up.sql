-- F34: stable idempotency key for billing events. A replay of an
-- already-committed usage row (crash between DB commit and spool rewrite)
-- collides on this unique index and is ignored instead of double-billing.
-- Nullable: legacy rows keep NULL (multiple NULLs do not collide); every
-- new row written by UsageService carries a UUID.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS event_id VARCHAR(36);
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_logs_event_id ON usage_logs(event_id);
