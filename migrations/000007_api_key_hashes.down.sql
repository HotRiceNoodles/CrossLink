-- WARNING: Rolling back this migration permanently deletes all rotated key hashes.
-- Active key hashes in api_keys.key_hash are preserved, but historical rotation data is lost.
DROP TABLE IF EXISTS api_key_hashes;
