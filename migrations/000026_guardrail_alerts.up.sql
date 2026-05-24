CREATE TABLE guardrail_alert_rules (
    id BIGSERIAL PRIMARY KEY,
    rule_id BIGINT NOT NULL REFERENCES guardrail_rules(id) ON DELETE CASCADE,
    team_id BIGINT,
    channels JSONB NOT NULL DEFAULT '[]',
    cooldown_minutes INT NOT NULL DEFAULT 5,
    enabled BOOLEAN NOT NULL DEFAULT true,
    last_triggered_at TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_alert_rules_rule_id UNIQUE (rule_id)
);

CREATE INDEX idx_alert_rules_team_id ON guardrail_alert_rules(team_id);

CREATE TABLE guardrail_alert_logs (
    id BIGSERIAL PRIMARY KEY,
    rule_id BIGINT NOT NULL,
    alert_rule_id BIGINT,
    rule_name VARCHAR(255),
    engine_type VARCHAR(50),
    severity VARCHAR(20),
    action VARCHAR(20),
    direction VARCHAR(10),
    reason VARCHAR(1000),
    model VARCHAR(255),
    content_preview VARCHAR(500),
    api_key_id BIGINT,
    team_id BIGINT,
    channels VARCHAR(500),
    status VARCHAR(20),
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_alert_logs_rule_id ON guardrail_alert_logs(rule_id);
CREATE INDEX idx_alert_logs_created_at ON guardrail_alert_logs(created_at);
CREATE INDEX idx_alert_logs_team_id ON guardrail_alert_logs(team_id);
