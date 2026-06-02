-- No schema changes. This migration exists to document that the DOWN path of
-- migration 000012 (configurable_rbac) can silently assign 'member' to users
-- whose role_id references a deleted custom role. A stored procedure or trigger
-- is not added here to avoid production risk. Instead, operators should:
-- 1. Never run DOWN on 000012 in production
-- 2. If rollback is required, first reassign users from custom roles to built-in roles
SELECT 1;
