-- 旁路检测服务（crosslink-guard，设计 DD-5/DD-6）：接入凭证、事件留存、trace 元数据（MySQL 方言）。
-- guard_events 只存脱敏摘要（SanitizePreview）与 SHA-256 哈希，不存原文。
CREATE TABLE IF NOT EXISTS guard_ingest_tokens (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id BIGINT NOT NULL,
    name VARCHAR(128) NOT NULL DEFAULT '',
    token_hash VARCHAR(64) NOT NULL,
    key_prefix VARCHAR(16) NOT NULL DEFAULT '',
    hash_algo VARCHAR(10) NOT NULL DEFAULT 'sha256',
    status TINYINT NOT NULL DEFAULT 1,
    last_used_at TIMESTAMP NULL,
    revoked_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uq_guard_ingest_tokens_hash (token_hash),
    INDEX idx_guard_ingest_tokens_org (org_id)
);

CREATE TABLE IF NOT EXISTS guard_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    trace_id VARCHAR(64) NOT NULL DEFAULT '',
    org_id BIGINT NOT NULL DEFAULT 0,
    rule_id BIGINT NOT NULL DEFAULT 0,
    rule_version BIGINT NOT NULL DEFAULT 0,
    engine_type VARCHAR(50) NOT NULL DEFAULT '',
    direction VARCHAR(10) NOT NULL,
    action VARCHAR(20) NOT NULL,
    severity VARCHAR(20) NOT NULL DEFAULT '',
    reason VARCHAR(1000) NOT NULL DEFAULT '',
    content_preview VARCHAR(500) NOT NULL DEFAULT '',
    content_sha256 VARCHAR(64) NOT NULL DEFAULT '',
    model VARCHAR(255) NOT NULL DEFAULT '',
    metadata JSON NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX idx_guard_events_org (org_id, created_at),
    INDEX idx_guard_events_trace (trace_id)
);

CREATE TABLE IF NOT EXISTS guard_traces (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    trace_id VARCHAR(64) NOT NULL,
    org_id BIGINT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'open',
    verdict VARCHAR(20) NOT NULL DEFAULT 'pending',
    request_at TIMESTAMP NULL,
    response_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uq_guard_traces_org_trace (org_id, trace_id),
    INDEX idx_guard_traces_org_created (org_id, created_at)
);
