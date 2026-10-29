-- Agent 工具集画像(Agent Toolset Profiling,Pro 及以上;设计 docs/design/agent-toolset.md)。
-- profiles:按 (api_key_id, tool_name) 的工具现状;events:变化史(first_seen / definition_changed)。
-- 隐私边界:永不存原始 schema JSON,仅名称、截断描述、sha256 与字节数。
CREATE TABLE IF NOT EXISTS agent_toolset_profiles (
    id                 BIGSERIAL PRIMARY KEY,
    org_id             BIGINT NOT NULL DEFAULT 0,
    team_id            BIGINT,
    api_key_id         BIGINT NOT NULL,
    tool_name          VARCHAR(256) NOT NULL,
    description        TEXT,
    schema_sha256      CHAR(64) NOT NULL DEFAULT '',
    schema_bytes       INT NOT NULL DEFAULT 0,
    definition_version INT NOT NULL DEFAULT 1,
    declared_count     BIGINT NOT NULL DEFAULT 0,
    invoked_count      BIGINT NOT NULL DEFAULT 0,
    first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_declared_at   TIMESTAMPTZ,
    last_invoked_at    TIMESTAMPTZ,
    last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_source        VARCHAR(16) NOT NULL DEFAULT 'proxy',
    last_model         VARCHAR(128) NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_toolset_profiles_key ON agent_toolset_profiles(api_key_id, tool_name);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_profiles_org ON agent_toolset_profiles(org_id);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_profiles_team ON agent_toolset_profiles(team_id);

CREATE TABLE IF NOT EXISTS agent_toolset_events (
    id                BIGSERIAL PRIMARY KEY,
    org_id            BIGINT NOT NULL DEFAULT 0,
    api_key_id        BIGINT NOT NULL,
    tool_name         VARCHAR(256) NOT NULL,
    event_type        VARCHAR(24) NOT NULL,
    old_schema_sha256 CHAR(64) NOT NULL DEFAULT '',
    new_schema_sha256 CHAR(64) NOT NULL DEFAULT '',
    source            VARCHAR(16) NOT NULL DEFAULT 'proxy',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_events_org ON agent_toolset_events(org_id, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_toolset_events_key ON agent_toolset_events(api_key_id, created_at);

-- RBAC:工具集查询动作(只读,admin/member/viewer 同 fingerprint:list 体例;
-- 采集与查询端点在商业 overlay,tier 动作 toolset:list 为 Pro 档)。
INSERT INTO role_permissions (role_id, action)
SELECT r.id, 'toolset:list'
FROM roles r WHERE r.name IN ('admin', 'member', 'viewer')
ON CONFLICT (role_id, action) DO NOTHING;
