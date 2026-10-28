-- 旁路检测服务（crosslink-guard，设计 DD-5/DD-6）：接入凭证、事件留存、trace 元数据（SQLite 方言）。
-- guard_events 只存脱敏摘要（SanitizePreview）与 SHA-256 哈希，不存原文。
CREATE TABLE IF NOT EXISTS guard_ingest_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id INTEGER NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL,
    key_prefix TEXT NOT NULL DEFAULT '',
    hash_algo TEXT NOT NULL DEFAULT 'sha256',
    status INTEGER NOT NULL DEFAULT 1,
    last_used_at DATETIME,
    revoked_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_guard_ingest_tokens_hash ON guard_ingest_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_guard_ingest_tokens_org ON guard_ingest_tokens(org_id);

CREATE TABLE IF NOT EXISTS guard_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id TEXT NOT NULL DEFAULT '',
    org_id INTEGER NOT NULL DEFAULT 0,
    rule_id INTEGER NOT NULL DEFAULT 0,
    rule_version INTEGER NOT NULL DEFAULT 0,
    engine_type TEXT NOT NULL DEFAULT '',
    direction TEXT NOT NULL,
    action TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    content_preview TEXT NOT NULL DEFAULT '',
    content_sha256 TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    metadata TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_guard_events_org ON guard_events(org_id, created_at);
CREATE INDEX IF NOT EXISTS idx_guard_events_trace ON guard_events(trace_id);

CREATE TABLE IF NOT EXISTS guard_traces (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id TEXT NOT NULL,
    org_id INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    verdict TEXT NOT NULL DEFAULT 'pending',
    request_at DATETIME,
    response_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_guard_traces_org_trace ON guard_traces(org_id, trace_id);
CREATE INDEX IF NOT EXISTS idx_guard_traces_org_created ON guard_traces(org_id, created_at);
