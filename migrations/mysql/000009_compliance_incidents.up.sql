-- 内容合规（商业版）：合规事件表与权限种子（MySQL 方言）。
CREATE TABLE IF NOT EXISTS compliance_incidents (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id BIGINT NULL,
    rule_id BIGINT NOT NULL DEFAULT 0,
    rule_version BIGINT NOT NULL DEFAULT 0,
    direction VARCHAR(10) NOT NULL,
    action_taken VARCHAR(20) NOT NULL,
    status VARCHAR(24) NOT NULL,
    categories JSON NOT NULL,
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
    reviewed_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_compliance_incidents_org (org_id, created_at),
    INDEX idx_compliance_incidents_status (status, created_at)
);

INSERT IGNORE INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:policy'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:check'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:logs'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:review'),
    ((SELECT id FROM roles WHERE name = 'member'), 'compliance:check'),
    ((SELECT id FROM roles WHERE name = 'member'), 'compliance:logs'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'compliance:logs');
