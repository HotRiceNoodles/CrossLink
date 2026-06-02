CREATE TABLE audit_logs (
    id             BIGSERIAL PRIMARY KEY,
    user_id        BIGINT NOT NULL,
    username       VARCHAR(64) NOT NULL,
    action         VARCHAR(64) NOT NULL,
    resource_type  VARCHAR(32) NOT NULL,
    resource_id    VARCHAR(64) NOT NULL,
    resource_name  VARCHAR(128),
    detail         JSONB,
    ip_address     VARCHAR(45),
    user_agent     VARCHAR(512),
    status         VARCHAR(16) NOT NULL DEFAULT 'success',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_logs_user_id ON audit_logs (user_id);
CREATE INDEX idx_audit_logs_action ON audit_logs (action);
CREATE INDEX idx_audit_logs_resource ON audit_logs (resource_type, resource_id);
CREATE INDEX idx_audit_logs_created_at ON audit_logs (created_at);

-- Seed audit permissions for admin role
INSERT INTO role_permissions (role_id, action)
SELECT id, 'audit:list' FROM roles WHERE name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;

INSERT INTO role_permissions (role_id, action)
SELECT id, 'audit:export' FROM roles WHERE name = 'admin'
ON CONFLICT (role_id, action) DO NOTHING;
