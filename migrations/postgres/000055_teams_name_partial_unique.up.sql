-- Replace the unique constraint/index on teams.name with a partial unique index
-- that excludes soft-deleted rows, allowing re-creation of teams with the same name.
ALTER TABLE teams DROP CONSTRAINT IF EXISTS teams_name_key;
DROP INDEX IF EXISTS teams_name_key;
CREATE UNIQUE INDEX teams_name_key ON teams (name) WHERE deleted_at IS NULL;
