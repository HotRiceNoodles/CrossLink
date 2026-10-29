-- Agent 工具集画像(Agent Toolset Profiling,Pro 及以上;设计 docs/design/agent-toolset.md)(MySQL 方言)。
-- profiles:按 (api_key_id, tool_name) 的工具现状;events:变化史(first_seen / definition_changed)。
-- 隐私边界:永不存原始 schema JSON,仅名称、截断描述、sha256 与字节数。
CREATE TABLE IF NOT EXISTS agent_toolset_profiles (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id BIGINT NOT NULL DEFAULT 0,
    team_id BIGINT NULL,
    api_key_id BIGINT NOT NULL,
    tool_name VARCHAR(256) NOT NULL,
    description TEXT NULL,
    schema_sha256 CHAR(64) NOT NULL DEFAULT '',
    schema_bytes INT NOT NULL DEFAULT 0,
    definition_version INT NOT NULL DEFAULT 1,
    declared_count BIGINT NOT NULL DEFAULT 0,
    invoked_count BIGINT NOT NULL DEFAULT 0,
    first_seen_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_declared_at TIMESTAMP NULL,
    last_invoked_at TIMESTAMP NULL,
    last_seen_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_source VARCHAR(16) NOT NULL DEFAULT 'proxy',
    last_model VARCHAR(128) NOT NULL DEFAULT '',
    UNIQUE KEY uq_agent_toolset_profiles_key (api_key_id, tool_name),
    INDEX idx_agent_toolset_profiles_org (org_id),
    INDEX idx_agent_toolset_profiles_team (team_id)
);

CREATE TABLE IF NOT EXISTS agent_toolset_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id BIGINT NOT NULL DEFAULT 0,
    api_key_id BIGINT NOT NULL,
    tool_name VARCHAR(256) NOT NULL,
    event_type VARCHAR(24) NOT NULL,
    old_schema_sha256 CHAR(64) NOT NULL DEFAULT '',
    new_schema_sha256 CHAR(64) NOT NULL DEFAULT '',
    source VARCHAR(16) NOT NULL DEFAULT 'proxy',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_agent_toolset_events_org (org_id, created_at),
    INDEX idx_agent_toolset_events_key (api_key_id, created_at)
);

-- RBAC:工具集查询动作(只读;采集与查询端点在商业 overlay,tier 动作为 Pro 档)。
INSERT IGNORE INTO role_permissions (role_id, action) VALUES
    ((SELECT id FROM roles WHERE name = 'admin'), 'toolset:list'),
    ((SELECT id FROM roles WHERE name = 'member'), 'toolset:list'),
    ((SELECT id FROM roles WHERE name = 'viewer'), 'toolset:list');
