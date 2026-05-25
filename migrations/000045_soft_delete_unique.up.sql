-- Replace the full-table unique constraint with a partial unique index
-- so that soft-deleted rows don't block re-creation of the same tuple.
ALTER TABLE provider_models DROP CONSTRAINT IF EXISTS provider_models_provider_id_model_name_provider_model_key;

CREATE UNIQUE INDEX IF NOT EXISTS provider_models_active_unique
    ON provider_models (provider_id, model_name, provider_model)
    WHERE deleted_at IS NULL;