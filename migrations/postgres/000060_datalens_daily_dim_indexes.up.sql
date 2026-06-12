-- Add dimension-specific partial indexes on daily metrics (matching the hourly table).
CREATE INDEX idx_ddm_model_day ON datalens_daily_metrics (org_id, agg_level, model_name, day_bucket) WHERE model_name IS NOT NULL;
CREATE INDEX idx_ddm_team_day ON datalens_daily_metrics (org_id, agg_level, team_id, day_bucket) WHERE team_id IS NOT NULL;
CREATE INDEX idx_ddm_key_day ON datalens_daily_metrics (org_id, agg_level, api_key_id, day_bucket) WHERE api_key_id IS NOT NULL;
CREATE INDEX idx_ddm_provider_day ON datalens_daily_metrics (org_id, agg_level, provider_id, day_bucket) WHERE provider_id IS NOT NULL;
