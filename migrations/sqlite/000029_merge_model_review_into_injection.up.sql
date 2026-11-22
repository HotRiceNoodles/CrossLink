-- 模型审核并入提示词注入规则（对照 postgres 000103）：只开模型判据。
UPDATE guardrail_rules
SET type = 'prompt_injection',
    config = json_set(
        COALESCE(config, '{}'),
        '$.rule_detection', json_object('enabled', json('false')),
        '$.model_review', json_object(
            'enabled', json('true'),
            'service_id', COALESCE(json_extract(config, '$.service_id'), 0),
            'incomplete', COALESCE(json_extract(config, '$.incomplete'), 'allow_alert'),
            'on_rule_hit', 'run'
        )
    )
WHERE type = 'model_review';
