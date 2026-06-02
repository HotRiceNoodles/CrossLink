-- Drop all tables in reverse dependency order (dependent tables first).
-- MySQL enforces FK constraints, so order matters.

DROP TABLE IF EXISTS mcp_tool_call_logs_archive;
DROP TABLE IF EXISTS mcp_tool_call_logs;
DROP TABLE IF EXISTS mcp_server_permissions;
DROP TABLE IF EXISTS mcp_servers;
DROP TABLE IF EXISTS agent_fingerprints;
DROP TABLE IF EXISTS optimization_actions;
DROP TABLE IF EXISTS insights;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS budget_requests;
DROP TABLE IF EXISTS budget_recommendations;
DROP TABLE IF EXISTS budget_snapshots;
DROP TABLE IF EXISTS budget_alerts;
DROP TABLE IF EXISTS guardrail_alert_logs;
DROP TABLE IF EXISTS guardrail_alert_rules;
DROP TABLE IF EXISTS guardrail_rules;
DROP TABLE IF EXISTS usage_logs;
DROP TABLE IF EXISTS api_key_hashes;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS provider_models;
DROP TABLE IF EXISTS providers;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS team_members;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS organization_members;
DROP TABLE IF EXISTS organizations;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS system_settings;
