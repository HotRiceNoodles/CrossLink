-- providers.name was a GLOBAL unique constraint (migration 000001), but providers
-- are soft-deleted (migration 000036). A soft-deleted row keeps its name and blocks
-- re-creation under the same name — onboarding / import / manual create all fail
-- with SQLSTATE 23505 against a name that "doesn't exist" (GORM filters soft-deleted
-- rows out of its queries but the DB constraint does not).
--
-- Convert to a PARTIAL unique index so only non-deleted rows enforce uniqueness.
-- Soft-deleted rows no longer occupy the name, and re-creating a provider with a
-- previously-used name becomes legal — GORM's default (deleted_at IS NULL) scope and
-- the DB constraint finally agree.
--
-- Note: CREATE INDEX CONCURRENTLY cannot run inside golang-migrate's transaction
-- wrapper; providers is a small table so a plain CREATE INDEX is fine here. For
-- very large deployments, run this migration manually with CONCURRENTLY offline.
ALTER TABLE providers DROP CONSTRAINT IF EXISTS providers_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS providers_name_key ON providers (name) WHERE deleted_at IS NULL;
