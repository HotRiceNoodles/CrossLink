DELETE FROM role_permissions WHERE action IN ('audit:list', 'audit:export');
DROP TABLE IF EXISTS audit_logs;
