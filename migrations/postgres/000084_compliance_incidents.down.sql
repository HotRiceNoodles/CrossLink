DELETE FROM role_permissions WHERE action IN ('compliance:policy', 'compliance:check', 'compliance:logs', 'compliance:review');
DROP TABLE IF EXISTS compliance_incidents;
