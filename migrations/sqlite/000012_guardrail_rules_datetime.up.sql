-- 修复：guardrail_rules.created_at/updated_at 在 sqlite 压缩基线（000001）里
-- 声明为 TEXT。glebarez 驱动按列声明类型决定转换：TEXT 列读出 string，无法
-- 扫描进 time.Time —— GuardrailService.LoadRules 全行扫描直接失败，sqlite
-- 部署的护栏因此静默失效（检测链 fail-open 放行，规则形同虚设）。
--
-- 重建两列为 DATETIME 声明；存量值经 datetime() 规范化为 UTC 文本（原值即
-- datetime('now') 默认，语义不变）。mysql 基线为 DATETIME(3)、postgres 为
-- TIMESTAMPTZ，本就是正确类型，无需对应迁移（三方言链已分歧，按各自链尾
-- 追加，见 docs/design/bypass-guard-service.md DD-6）。
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
    created_at DATETIME,
    updated_at DATETIME
);
INSERT INTO guardrail_rules_rebuild (id, name, type, direction, enabled, config, severity, action, model_filter, org_id, version, created_at, updated_at)
SELECT id, name, type, direction, enabled, config, severity, action, model_filter, org_id, version,
       datetime(created_at), datetime(updated_at)
FROM guardrail_rules;
DROP TABLE guardrail_rules;
ALTER TABLE guardrail_rules_rebuild RENAME TO guardrail_rules;
CREATE INDEX IF NOT EXISTS idx_guardrail_rules_type ON guardrail_rules(type);
CREATE INDEX IF NOT EXISTS idx_guardrail_rules_enabled ON guardrail_rules(enabled) WHERE enabled = 1;
