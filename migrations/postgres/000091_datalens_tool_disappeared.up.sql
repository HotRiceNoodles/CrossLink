-- DataLens tool dimension: disappeared event count (agent_toolset_events
-- event_type='disappeared'), symmetric with first_seen/definition_changed.
ALTER TABLE datalens_tool_daily_metrics ADD COLUMN disappeared_count INT NOT NULL DEFAULT 0;
