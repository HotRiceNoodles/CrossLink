-- Reversible: drops the page-configured allowlist override table. After the
-- downgrade the effective policy falls back to env/YAML/strict on restart.
DROP TABLE IF EXISTS ssrf_allowlist_configs;
