-- API Key budget fields
ALTER TABLE api_keys ADD COLUMN max_budget DECIMAL(12,4) NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN budget_period VARCHAR(16) NOT NULL DEFAULT 'monthly';

-- Budget alerts table
CREATE TABLE budget_alerts (
    id BIGSERIAL PRIMARY KEY,
    team_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
    key_id BIGINT REFERENCES api_keys(id) ON DELETE CASCADE,
    threshold_pct SMALLINT NOT NULL,
    webhook_url VARCHAR(512) NOT NULL,
    last_triggered_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_alert_target CHECK ((team_id IS NULL) <> (key_id IS NULL)),
    CONSTRAINT chk_alert_threshold CHECK (threshold_pct > 0 AND threshold_pct <= 100)
);

-- Budget snapshots for calibration history
CREATE TABLE budget_snapshots (
    id BIGSERIAL PRIMARY KEY,
    target_type VARCHAR(16) NOT NULL,
    target_id BIGINT NOT NULL,
    period_key VARCHAR(16) NOT NULL,
    spent DECIMAL(16,8) NOT NULL DEFAULT 0,
    budget DECIMAL(12,4) NOT NULL DEFAULT 0,
    currency VARCHAR(3) NOT NULL DEFAULT 'CNY',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (target_type, target_id, period_key)
);

CREATE INDEX idx_budget_alerts_team ON budget_alerts(team_id);
CREATE INDEX idx_budget_alerts_key ON budget_alerts(key_id);
