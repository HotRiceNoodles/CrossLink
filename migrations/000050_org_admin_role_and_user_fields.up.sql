-- 000050: Add email + force_password_change to users, create org_admin system role
ALTER TABLE users ADD COLUMN email VARCHAR(255);
ALTER TABLE users ADD COLUMN force_password_change BOOLEAN NOT NULL DEFAULT FALSE;

-- Create org_admin system role
INSERT INTO roles (name, display_name, is_system, created_at)
VALUES ('org_admin', 'Organization Admin', true, NOW())
ON CONFLICT (name) DO NOTHING;

-- Insert permissions for org_admin (full control within org, no system management)
-- NOTE: when new ValidActions are added in future versions, a follow-up migration
-- (similar to 000049_role_permissions_sync) should be created to sync org_admin permissions.
INSERT INTO role_permissions (role_id, action)
SELECT r.id, v.action FROM roles r, (VALUES
  -- Org management
  ('org:list'), ('org:update'), ('org:manage_members'), ('org:view_billing'), ('org:manage_billing'),
  -- User management (org-scoped)
  ('user:list'), ('user:create'), ('user:update'), ('user:delete'),
  -- Role management (org-scoped)
  ('role:list'), ('role:create'), ('role:update'), ('role:delete'),
  -- Key management (org-scoped)
  ('key:list'), ('key:create'), ('key:update'), ('key:delete'), ('key:regenerate'), ('key:rotate'), ('key:hashes'),
  -- Team management (org-scoped)
  ('team:list'), ('team:create'), ('team:update'), ('team:delete'), ('team:manage_members'),
  -- Usage & budget
  ('usage:list'), ('usage:export'), ('usage:stats'), ('budget:manage'),
  -- Guardrails (org-scoped)
  ('guardrail:list'), ('guardrail:create'), ('guardrail:update'), ('guardrail:delete'), ('guardrail:test'),
  ('guardrail_alert:list'), ('guardrail_alert:create'), ('guardrail_alert:update'), ('guardrail_alert:delete'), ('guardrail_alert:logs'),
  -- Audit & insights (org-scoped)
  ('audit:list'), ('audit:export'), ('insight:manage'),
  -- Playground
  ('playground:use'),
  -- Secrets (org-scoped)
  ('secret:test'), ('secret:manage'),
  -- MCP (read + org-scoped management)
  ('mcp:list'), ('mcp:view'), ('mcp:create'), ('mcp:update'), ('mcp:delete'), ('mcp:permission'), ('mcp:logs'), ('mcp:stats'),
  -- Read-only access to system resources (needed for key/team creation)
  ('provider:list'), ('model:list'),
  -- Self-service
  ('system:password')
) AS v(action)
WHERE r.name = 'org_admin'
ON CONFLICT DO NOTHING;
