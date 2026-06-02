-- Optimization actions: actionable suggestions with apply/dismiss workflow
CREATE TABLE IF NOT EXISTS optimization_actions (
    id BIGSERIAL PRIMARY KEY,
    action_type VARCHAR(32) NOT NULL,                    -- model_switch, cache_enable, rate_adjust
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    priority VARCHAR(16) NOT NULL DEFAULT 'medium',      -- high, medium, low
    status VARCHAR(16) NOT NULL DEFAULT 'pending',       -- pending, applied, dismissed
    payload JSONB NOT NULL DEFAULT '{}',                  -- structured action data
    saving_estimate DECIMAL(12,2) DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    applied_at TIMESTAMPTZ,
    applied_by BIGINT,
    dismissed_at TIMESTAMPTZ,
    dismissed_by BIGINT
);

CREATE INDEX idx_opt_actions_status ON optimization_actions(status);
CREATE INDEX idx_opt_actions_type ON optimization_actions(action_type);
CREATE INDEX idx_opt_actions_priority ON optimization_actions(priority);
CREATE INDEX idx_opt_actions_created ON optimization_actions(created_at DESC);
