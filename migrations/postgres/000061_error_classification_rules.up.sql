-- Error classification rules (global, platform-level config for failover precision)
CREATE TABLE error_classification_rules (
    id             BIGSERIAL PRIMARY KEY,
    match_field    VARCHAR(16) NOT NULL,
    pattern        VARCHAR(128) NOT NULL,
    classification VARCHAR(16) NOT NULL DEFAULT 'quota',
    provider_type  VARCHAR(32),
    scope          VARCHAR(16) NOT NULL DEFAULT 'account',
    priority       INT NOT NULL DEFAULT 100,
    enabled        BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_error_rules_enabled ON error_classification_rules(enabled);

-- Seed persistent error classification rules
INSERT INTO error_classification_rules (match_field, pattern, provider_type, scope) VALUES
    ('code', 'insufficient_quota',         'openai_compatible', 'account'),
    ('code', 'quota_exceeded',             'openai_compatible', 'account'),
    ('code', 'billing_hard_limit_reached', 'openai_compatible', 'account'),
    ('type', 'model_deprecated',           'openai_compatible', 'model'),
    ('type', 'billing_disabled',           'anthropic',         'account'),
    ('status', '402',                      NULL,                'account');

-- Grant error_rule actions to the admin (super-admin) role
INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'error_rule:list'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'error_rule:create'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'error_rule:update'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'error_rule:delete')
ON CONFLICT DO NOTHING;
