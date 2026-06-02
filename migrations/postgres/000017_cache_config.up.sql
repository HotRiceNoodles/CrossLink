INSERT INTO system_settings (key, value, updated_at) VALUES
    ('cache_enabled', 'true', NOW()),
    ('cache_default_ttl', '5m', NOW()),
    ('cache_embeddings_ttl', '60m', NOW())
ON CONFLICT (key) DO NOTHING;
