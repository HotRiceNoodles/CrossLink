-- Remove error_rule permissions from the shared role_permissions table (DELETE, not DROP)
DELETE FROM role_permissions WHERE action LIKE 'error_rule:%';
DROP TABLE IF EXISTS error_classification_rules;
