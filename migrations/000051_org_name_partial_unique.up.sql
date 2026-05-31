-- Replace plain UNIQUE on organizations.name with a partial unique index
-- that only applies to non-deleted rows, so soft-deleted names can be reused.
ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_name_key;
CREATE UNIQUE INDEX organizations_name_active_idx ON organizations (name) WHERE deleted_at IS NULL;
