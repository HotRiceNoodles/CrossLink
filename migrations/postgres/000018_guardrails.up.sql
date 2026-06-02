CREATE TABLE guardrail_rules (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    type VARCHAR(50) NOT NULL,
    direction VARCHAR(10) NOT NULL,
    enabled BOOLEAN DEFAULT true,
    config JSONB NOT NULL,
    severity VARCHAR(20) DEFAULT 'medium',
    action VARCHAR(20) DEFAULT 'block',
    model_filter TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_guardrail_rules_type ON guardrail_rules(type);
CREATE INDEX idx_guardrail_rules_enabled ON guardrail_rules(enabled) WHERE enabled = true;
