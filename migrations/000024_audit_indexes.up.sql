-- Composite indexes for common audit log query patterns
CREATE INDEX idx_audit_logs_action_created ON audit_logs (action, created_at DESC);
CREATE INDEX idx_audit_logs_resource_type_created ON audit_logs (resource_type, created_at DESC);
