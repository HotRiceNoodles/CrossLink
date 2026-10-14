-- B5 (2026-10-14): singleton row for the page-configured SSRF internal
-- allowlist override (sqlite/dev dialect). Same semantics as postgres
-- 000081: presence overrides env/YAML; '[]' is explicit strict.
CREATE TABLE IF NOT EXISTS ssrf_allowlist_configs (
    id INTEGER PRIMARY KEY,
    cidrs TEXT NOT NULL,
    version INTEGER NOT NULL,
    removed BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by INTEGER NOT NULL DEFAULT 0,
    updated_by_name TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
