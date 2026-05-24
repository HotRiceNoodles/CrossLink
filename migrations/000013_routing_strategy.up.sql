-- provider_models: add routing strategy column
ALTER TABLE provider_models ADD COLUMN routing_strategy VARCHAR(32) NOT NULL DEFAULT 'weighted_random';

-- validate routing strategy values
ALTER TABLE provider_models ADD CONSTRAINT chk_routing_strategy
    CHECK (routing_strategy IN ('weighted_random', 'round_robin', 'least_latency', 'least_cost', 'canary'));
