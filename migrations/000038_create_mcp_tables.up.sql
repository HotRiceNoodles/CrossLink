-- MCP Server registry
CREATE TABLE mcp_servers (
    id               BIGSERIAL PRIMARY KEY,
    name             VARCHAR(64) NOT NULL UNIQUE,
    display_name     VARCHAR(128),
    description      VARCHAR(512),
    transport_type   VARCHAR(16) NOT NULL CHECK (transport_type IN ('http', 'sse', 'stdio')),
    url              VARCHAR(512),
    stdio_config     JSONB,
    auth_type        VARCHAR(32) DEFAULT 'none' CHECK (auth_type IN ('none', 'bearer', 'basic', 'oauth2', 'sigv4')),
    auth_config      JSONB,
    custom_headers   JSONB DEFAULT '{}',
    status           SMALLINT NOT NULL DEFAULT 1,
    health_status    SMALLINT NOT NULL DEFAULT 0,
    last_health_check TIMESTAMPTZ,
    tool_count       INT DEFAULT 0,
    enabled          BOOLEAN DEFAULT TRUE,
    tier_required    VARCHAR(16) DEFAULT 'community',
    created_by       BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,

    CONSTRAINT chk_transport_config CHECK (
        (transport_type = 'stdio' AND stdio_config IS NOT NULL) OR
        (transport_type IN ('http', 'sse') AND url IS NOT NULL)
    )
);

CREATE INDEX idx_mcp_servers_status ON mcp_servers(status);
CREATE INDEX idx_mcp_servers_enabled ON mcp_servers(enabled);
CREATE INDEX idx_mcp_servers_deleted ON mcp_servers(deleted_at);

-- MCP Server permissions
CREATE TABLE mcp_server_permissions (
    id             BIGSERIAL PRIMARY KEY,
    server_id      BIGINT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    principal_type VARCHAR(16) NOT NULL CHECK (principal_type IN ('key', 'team', 'role')),
    principal_id   BIGINT NOT NULL,
    allow_tools    JSONB DEFAULT '["*"]',
    deny_tools     JSONB DEFAULT '[]',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_mcp_perm UNIQUE (server_id, principal_type, principal_id)
);

CREATE INDEX idx_mcp_perm_principal ON mcp_server_permissions(principal_type, principal_id);

-- MCP tool call logs (monthly partitioned)
CREATE TABLE mcp_tool_call_logs (
    id           BIGSERIAL,
    request_id   VARCHAR(36),
    server_id    BIGINT,
    server_name  VARCHAR(64),
    tool_name    VARCHAR(128),
    method       VARCHAR(32),
    input_size   INT DEFAULT 0,
    output_size  INT DEFAULT 0,
    duration     INT DEFAULT 0,
    status       SMALLINT NOT NULL,
    error_code   INT,
    error_msg    VARCHAR(512),
    api_key_id   BIGINT,
    user_id      BIGINT,
    team_id      BIGINT,
    blocked_by   VARCHAR(64),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX idx_mcp_logs_server ON mcp_tool_call_logs(server_id);
CREATE INDEX idx_mcp_logs_key ON mcp_tool_call_logs(api_key_id);
CREATE INDEX idx_mcp_logs_team ON mcp_tool_call_logs(team_id);
CREATE INDEX idx_mcp_logs_time ON mcp_tool_call_logs(created_at);
