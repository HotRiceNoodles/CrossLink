-- 模型审核并入提示词注入规则（对照 postgres 000103）：只开模型判据。
UPDATE guardrail_rules
SET type = 'prompt_injection',
    config = JSON_SET(
        COALESCE(config, JSON_OBJECT()),
        '$.rule_detection', JSON_OBJECT('enabled', FALSE),
        '$.model_review', JSON_OBJECT(
            'enabled', TRUE,
            'service_id', CAST(COALESCE(JSON_EXTRACT(config, '$.service_id'), 0) AS SIGNED),
            'incomplete', COALESCE(JSON_UNQUOTE(JSON_EXTRACT(config, '$.incomplete')), 'allow_alert'),
            'on_rule_hit', 'run'
        )
    )
WHERE type = 'model_review';
