-- Revert: drop the partial unique index and restore the original global unique
-- constraint on providers.name (restores the pre-000067 behavior, including the
-- soft-delete collision). Only run if rolling back.
DROP INDEX IF EXISTS providers_name_key;
ALTER TABLE providers ADD CONSTRAINT providers_name_key UNIQUE (name);
