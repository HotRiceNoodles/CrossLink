DROP INDEX IF EXISTS organizations_name_active_idx;
ALTER TABLE organizations ADD CONSTRAINT organizations_name_key UNIQUE (name);
