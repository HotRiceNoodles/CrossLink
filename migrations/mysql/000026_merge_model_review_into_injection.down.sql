UPDATE guardrail_rules
SET type = 'model_review',
    config = JSON_OBJECT(
        'service_id', CAST(COALESCE(JSON_EXTRACT(config, '$.model_review.service_id'), 0) AS SIGNED),
        'incomplete', COALESCE(JSON_UNQUOTE(JSON_EXTRACT(config, '$.model_review.incomplete')), 'allow_alert')
    )
WHERE type = 'prompt_injection'
  AND JSON_UNQUOTE(JSON_EXTRACT(config, '$.model_review.enabled')) = 'true'
  AND COALESCE(JSON_EXTRACT(config, '$.rule_detection.enabled'), TRUE) = FALSE;
