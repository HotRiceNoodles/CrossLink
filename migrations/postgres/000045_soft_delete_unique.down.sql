DROP INDEX IF EXISTS provider_models_active_unique;

ALTER TABLE provider_models ADD CONSTRAINT provider_models_provider_id_model_name_provider_model_key
    UNIQUE (provider_id, model_name, provider_model);