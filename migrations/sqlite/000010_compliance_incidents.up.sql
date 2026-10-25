-- 内容合规（商业版）：合规事件表与权限种子（SQLite 方言）。
CREATE TABLE IF NOT EXISTS compliance_incidents (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id INTEGER,
    rule_id INTEGER NOT NULL DEFAULT 0,
    rule_version INTEGER NOT NULL DEFAULT 0,
    direction TEXT NOT NULL,
    action_taken TEXT NOT NULL,
    status TEXT NOT NULL,
    categories TEXT NOT NULL DEFAULT '[]',
    max_risk_level TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    content_sha256 TEXT NOT NULL DEFAULT '',
    content_excerpt TEXT NOT NULL DEFAULT '',
    engine_error TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    api_key_id INTEGER NOT NULL DEFAULT 0,
    team_id INTEGER NOT NULL DEFAULT 0,
    reviewer_id INTEGER NOT NULL DEFAULT 0,
    reviewer_name TEXT NOT NULL DEFAULT '',
    review_note TEXT NOT NULL DEFAULT '',
    reviewed_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_compliance_incidents_org ON compliance_incidents(org_id, created_at);
CREATE INDEX IF NOT EXISTS idx_compliance_incidents_status ON compliance_incidents(status, created_at);

INSERT OR IGNORE INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:policy'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:check'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:logs'),
    ((SELECT id FROM roles WHERE name = 'admin'), 'compliance:review'),
    ((SELECT id FROM roles WHERE name = 'member'), 'compliance:check'),
    ((SELECT id FROM roles WHERE name = 'member'), 'compliance:logs'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'compliance:logs');
