-- Rollback agent fingerprint library

DELETE FROM role_permissions WHERE action IN ('fingerprint:list', 'fingerprint:view', 'fingerprint:manage');

DROP INDEX IF EXISTS idx_agent_fingerprints_dedup;
DROP INDEX IF EXISTS idx_agent_fingerprints_status_type;
DROP INDEX IF EXISTS idx_agent_fingerprints_origin;
DROP INDEX IF EXISTS idx_agent_fingerprints_name;
DROP INDEX IF EXISTS idx_agent_fingerprints_status;

DROP TABLE IF EXISTS agent_fingerprints;
