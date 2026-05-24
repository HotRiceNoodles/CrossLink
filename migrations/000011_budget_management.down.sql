DROP TABLE IF EXISTS budget_snapshots;
DROP TABLE IF EXISTS budget_alerts;
ALTER TABLE api_keys DROP COLUMN IF EXISTS max_budget;
ALTER TABLE api_keys DROP COLUMN IF EXISTS budget_period;
