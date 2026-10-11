-- Zero-change key migration (docs/plans/2026-10-11-zero-key-migration.md):
-- imported vendor keys are registered as-is (equal-value keys) so existing
-- apps only switch base_url. key_source marks the transitional state;
-- zero-grace rotation flips it back to native.
ALTER TABLE api_keys ADD COLUMN key_source VARCHAR(16) NOT NULL DEFAULT 'native';
ALTER TABLE api_keys ADD COLUMN source_vendor VARCHAR(32) NOT NULL DEFAULT '';
