-- 内容合规（商业版）：合规事件表与权限种子。
-- 表只存脱敏数据：excerpt 为引擎脱敏摘录，sha256 为不可逆哈希。
CREATE TABLE IF NOT EXISTS compliance_incidents (
    id BIGSERIAL PRIMARY KEY,
    org_id BIGINT,
    rule_id BIGINT NOT NULL DEFAULT 0,
    rule_version BIGINT NOT NULL DEFAULT 0,
    direction VARCHAR(10) NOT NULL,
    action_taken VARCHAR(20) NOT NULL,
    status VARCHAR(24) NOT NULL,
    categories JSONB NOT NULL DEFAULT '[]',
    max_risk_level VARCHAR(20) NOT NULL DEFAULT '',
    reason VARCHAR(1000) NOT NULL DEFAULT '',
    content_sha256 VARCHAR(64) NOT NULL DEFAULT '',
    content_excerpt VARCHAR(500) NOT NULL DEFAULT '',
    engine_error VARCHAR(500) NOT NULL DEFAULT '',
    model VARCHAR(255) NOT NULL DEFAULT '',
    api_key_id BIGINT NOT NULL DEFAULT 0,
    team_id BIGINT NOT NULL DEFAULT 0,
    reviewer_id BIGINT NOT NULL DEFAULT 0,
    reviewer_name VARCHAR(64) NOT NULL DEFAULT '',
    review_note VARCHAR(500) NOT NULL DEFAULT '',
    reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_compliance_incidents_org ON compliance_incidents(org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_compliance_incidents_status ON compliance_incidents(status, created_at DESC);

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:policy'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:check'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:logs'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:review')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'member'), 'compliance:check'),
    ((SELECT id FROM roles WHERE name = 'member'), 'compliance:logs')
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'viewer'), 'compliance:logs')
ON CONFLICT (role_id, action) DO NOTHING;
