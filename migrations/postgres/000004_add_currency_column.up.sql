-- Single-currency design (2026-11): provider_models no longer carries a
-- per-model currency — every price is denominated in the deployment's one
-- write-once settlement currency (system_settings.billing_currency, fixed at
-- first install). This migration is now a no-op; usage_logs keeps its
-- currency column for row-level lineage (see 000005).
SELECT 1;
