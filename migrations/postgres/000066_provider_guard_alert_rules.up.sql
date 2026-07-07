-- Provider guardrail alert rules (B1 Enterprise).
-- Per-(org, provider, model, limit_type) alert rules with multi-channel
-- notification + cooldown. Populated via the commercial overlay's admin CRUD
-- (Enterprise-gated). The dispatch path (ProviderGuardAlertService) looks up
-- matching rules on each guardrail exceedance; if none match, falls back to the
-- global webhook (B1 MVP).
CREATE TABLE provider_guard_alert_rules (
    id              BIGSERIAL PRIMARY KEY,
    org_id          BIGINT,
    provider_name   VARCHAR(64)  NOT NULL DEFAULT '',
    model_name      VARCHAR(128) NOT NULL DEFAULT '',
    limit_type      VARCHAR(16)  NOT NULL DEFAULT '',  -- 'conc' | 'rpm' | '' = any
    channels        JSONB        NOT NULL DEFAULT '[]',
    cooldown_seconds INT         NOT NULL DEFAULT 300,
    enabled         BOOLEAN      NOT NULL DEFAULT true,
    last_triggered_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_pgar_org_enabled ON provider_guard_alert_rules (org_id, enabled);
