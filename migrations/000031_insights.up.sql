-- LLM-generated insights (cached to avoid re-generation)
CREATE TABLE IF NOT EXISTS insights (
    id BIGSERIAL PRIMARY KEY,
    period VARCHAR(16) NOT NULL DEFAULT 'monthly',       -- weekly or monthly
    period_key VARCHAR(16) NOT NULL,                      -- e.g. '2026-05' or '2026-W19'
    scope VARCHAR(16) NOT NULL DEFAULT 'global',          -- global, team, key
    scope_id BIGINT NOT NULL DEFAULT 0,                   -- 0 for global
    insight_type VARCHAR(32) NOT NULL DEFAULT 'summary',  -- summary, cost, efficiency, anomaly, recommendation
    title VARCHAR(256) NOT NULL DEFAULT '',
    content TEXT NOT NULL,                                 -- LLM-generated natural language insight
    model_used VARCHAR(128) NOT NULL DEFAULT '',           -- which model generated this insight
    tokens_used INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_insights_period ON insights(period_key DESC);
CREATE INDEX idx_insights_scope ON insights(scope, scope_id);
CREATE INDEX idx_insights_type ON insights(insight_type);
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);
