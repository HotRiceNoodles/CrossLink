-- 模型审核并入提示词注入规则：历史 type=model_review 的行转成复合规则，
-- 只开模型判据（rule_detection.enabled=false），保留原服务、未完成策略与规则范围。
UPDATE guardrail_rules
SET type = 'prompt_injection',
    config = jsonb_set(
        jsonb_set(
            COALESCE(config, '{}'::jsonb),
            '{model_review}',
            jsonb_build_object(
                'enabled', true,
                'service_id', COALESCE((config->>'service_id')::bigint, 0),
                'incomplete', COALESCE(NULLIF(config->>'incomplete', ''), 'allow_alert'),
                'on_rule_hit', 'run'
            ),
            true
        ),
        '{rule_detection}',
        '{"enabled": false}'::jsonb,
        true
    )
WHERE type = 'model_review';
