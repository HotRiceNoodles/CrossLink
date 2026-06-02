-- WARNING: If any rows have hash_algo='sm3', dropping this column will make those hashes unverifiable.
-- Consider rotating SM3-hashed API keys before rolling back.
ALTER TABLE api_key_hashes DROP COLUMN IF EXISTS hash_algo;
