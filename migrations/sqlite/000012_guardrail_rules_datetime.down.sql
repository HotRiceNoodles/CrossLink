-- 回滚：guardrail_rules 时间列恢复 TEXT 声明（回到基线形态）。
-- 值经 strftime 规范回 'YYYY-MM-DD HH:MM:SS' 文本；DATETIME 值本就以文本
-- 存储，往返无损（亚秒精度舍弃）。
CREATE TABLE guardrail_rules_rebuild (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL,
    direction  TEXT NOT NULL,
    enabled    INTEGER DEFAULT 1,
    config     TEXT NOT NULL,
    severity   TEXT DEFAULT 'medium',
    action     TEXT DEFAULT 'block',
    model_filter TEXT,
    org_id     INTEGER,
    version    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT,
    updated_at TEXT
);
INSERT INTO guardrail_rules_rebuild (id, name, type, direction, enabled, config, severity, action, model_filter, org_id, version, created_at, updated_at)
SELECT id, name, type, direction, enabled, config, severity, action, model_filter, org_id, version,
       strftime('%Y-%m-%d %H:%M:%S', created_at), strftime('%Y-%m-%d %H:%M:%S', updated_at)
FROM guardrail_rules;
DROP TABLE guardrail_rules;
ALTER TABLE guardrail_rules_rebuild RENAME TO guardrail_rules;
CREATE INDEX IF NOT EXISTS idx_guardrail_rules_type ON guardrail_rules(type);
CREATE INDEX IF NOT EXISTS idx_guardrail_rules_enabled ON guardrail_rules(enabled) WHERE enabled = 1;
