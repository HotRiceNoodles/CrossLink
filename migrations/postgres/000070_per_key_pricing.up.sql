-- per-Key price multiplier and billable cost tracking.
-- price_multiplier on api_keys: markup applied per key (1.0 = no markup).
-- billable_cost on usage_logs: upstream cost × key multiplier (stored for reconciliation).
-- See docs/plans/2026-07-17-per-key-pricing-reconciliation-design.md.

ALTER TABLE api_keys ADD COLUMN price_multiplier DECIMAL(6,4) NOT NULL DEFAULT 1.0000;
ALTER TABLE usage_logs ADD COLUMN billable_cost DECIMAL(16,8) NOT NULL DEFAULT 0;
