-- Drop indexes
DROP INDEX IF EXISTS idx_api_keys_created_by_id;
DROP INDEX IF EXISTS idx_api_keys_team_id;
DROP INDEX IF EXISTS idx_usage_logs_team_id;
DROP INDEX IF EXISTS idx_teams_created_by_id;

-- Revert FKs to original (no ON DELETE)
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_created_by_id_fkey;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_created_by_id_fkey
    FOREIGN KEY (created_by_id) REFERENCES users(id);

ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_team_id_fkey;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_team_id_fkey
    FOREIGN KEY (team_id) REFERENCES teams(id);

ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS usage_logs_team_id_fkey;
ALTER TABLE usage_logs ADD CONSTRAINT usage_logs_team_id_fkey
    FOREIGN KEY (team_id) REFERENCES teams(id);
