-- Add indexes on FK columns added in 000009
CREATE INDEX IF NOT EXISTS idx_api_keys_created_by_id ON api_keys(created_by_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_team_id ON api_keys(team_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_team_id ON usage_logs(team_id);
CREATE INDEX IF NOT EXISTS idx_teams_created_by_id ON teams(created_by_id);

-- Recreate FKs with ON DELETE SET NULL where appropriate
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_created_by_id_fkey;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_created_by_id_fkey
    FOREIGN KEY (created_by_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_team_id_fkey;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_team_id_fkey
    FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE SET NULL;

ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_team_id_fkey;
ALTER TABLE usage_logs ADD CONSTRAINT usage_logs_team_id_fkey
    FOREIGN KEY (team_id) REFERENCES teams(id) ON DELETE SET NULL;
