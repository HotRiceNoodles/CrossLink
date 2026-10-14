-- B5 (2026-10-14): singleton row for the page-configured SSRF internal
-- allowlist override. Row presence overrides env/YAML; cidrs='[]' is an
-- explicit strict policy distinct from absence. version is the
-- optimistic-lock counter; the row is only ever written through
-- compare-and-swap updates. removed=true is a TOMBSTONE (reset marker):
-- the row is intentionally kept so version stays monotonically increasing —
-- a physical delete would reset versions and admit ABA overwrites.
CREATE TABLE IF NOT EXISTS ssrf_allowlist_configs (
    id BIGINT PRIMARY KEY,
    cidrs TEXT NOT NULL,
    version BIGINT NOT NULL,
    removed BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by BIGINT NOT NULL DEFAULT 0,
    updated_by_name VARCHAR(64) NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
