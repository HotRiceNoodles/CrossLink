-- Agent 工具集画像(Agent Toolset Profiling,Pro 及以上;设计 docs/design/agent-toolset.md)(SQLite 方言)。
-- profiles:按 (api_key_id, tool_name) 的工具现状;events:变化史(first_seen / definition_changed)。
-- 隐私边界:永不存原始 schema JSON,仅名称、截断描述、sha256 与字节数。
CREATE TABLE IF NOT EXISTS agent_toolset_profiles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id INTEGER NOT NULL DEFAULT 0,
    team_id INTEGER,
    api_key_id INTEGER NOT NULL,
    tool_name TEXT NOT NULL,
    description TEXT,
    schema_sha256 TEXT NOT NULL DEFAULT '',
    schema_bytes INTEGER NOT NULL DEFAULT 0,
    definition_version INTEGER NOT NULL DEFAULT 1,
    declared_count INTEGER NOT NULL DEFAULT 0,
    invoked_count INTEGER NOT NULL DEFAULT 0,
    first_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_declared_at DATETIME,
    last_invoked_at DATETIME,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_source TEXT NOT NULL DEFAULT 'proxy',
    last_model TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_toolset_profiles_key ON agent_toolset_profiles(api_key_id, tool_name);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_profiles_org ON agent_toolset_profiles(org_id);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_profiles_team ON agent_toolset_profiles(team_id);

CREATE TABLE IF NOT EXISTS agent_toolset_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id INTEGER NOT NULL DEFAULT 0,
    api_key_id INTEGER NOT NULL,
    tool_name TEXT NOT NULL,
    event_type TEXT NOT NULL,
    old_schema_sha256 TEXT NOT NULL DEFAULT '',
    new_schema_sha256 TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'proxy',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_events_org ON agent_toolset_events(org_id, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_events_key ON agent_toolset_events(api_key_id, created_at);

-- RBAC:工具集查询动作(只读;采集与查询端点在商业 overlay,tier 动作为 Pro 档)。
INSERT OR IGNORE INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'toolset:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'toolset:list');
