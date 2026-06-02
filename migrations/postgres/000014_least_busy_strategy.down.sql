ALTER TABLE provider_models DROP CONSTRAINT chk_routing_strategy;
ALTER TABLE provider_models ADD CONSTRAINT chk_routing_strategy
    CHECK (routing_strategy IN ('weighted_random', 'round_robin', 'least_latency', 'least_cost', 'canary'));
