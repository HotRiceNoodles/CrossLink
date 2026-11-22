UPDATE guardrail_rules
SET type = 'model_review',
    config = jsonb_build_object(
        'service_id', COALESCE((config->'model_review'->>'service_id')::bigint, 0),
        'incomplete', COALESCE(NULLIF(config->'model_review'->>'incomplete', ''), 'allow_alert')
    )
WHERE type = 'prompt_injection'
  AND config->'model_review'->>'enabled' = 'true'
  AND COALESCE((config->'rule_detection'->>'enabled')::boolean, true) = false;
