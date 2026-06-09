DROP INDEX IF EXISTS teams_name_key;
CREATE UNIQUE INDEX teams_name_key ON teams (name);
