ALTER TABLE provider_models DROP CONSTRAINT IF EXISTS chk_routing_strategy;
ALTER TABLE provider_models DROP COLUMN IF EXISTS routing_strategy;
