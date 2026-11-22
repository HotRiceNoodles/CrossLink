UPDATE guardrail_rules
SET type = 'model_review',
    config = json_object(
        'service_id', COALESCE(json_extract(config, '$.model_review.service_id'), 0),
        'incomplete', COALESCE(json_extract(config, '$.model_review.incomplete'), 'allow_alert')
    )
WHERE type = 'prompt_injection'
  AND json_extract(config, '$.model_review.enabled') = 1
  AND COALESCE(json_extract(config, '$.rule_detection.enabled'), 1) = 0;
