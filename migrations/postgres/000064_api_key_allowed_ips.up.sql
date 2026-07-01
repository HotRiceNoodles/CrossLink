-- Add AllowedIPs field to APIKey: JSON array of IP/CIDR strings; null/empty = no binding.
ALTER TABLE api_keys ADD COLUMN allowed_ips JSONB;
