-- Toolset disappeared events (design §5.1 defer closed): profile rows gain a
-- present flag instead of being deleted on disappearance, so reappearing
-- keeps definition_version continuity and never re-emits first_seen.
ALTER TABLE agent_toolset_profiles ADD COLUMN present BOOLEAN NOT NULL DEFAULT TRUE;
