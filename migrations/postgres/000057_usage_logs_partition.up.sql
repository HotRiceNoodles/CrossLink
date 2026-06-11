-- Partition migration is handled by the CLI command 'crosslink migrate-partition'.
-- This migration creates the tracking marker only.
-- The actual usage_logs partition conversion must be performed manually
-- during a maintenance window using the CLI command.
CREATE TABLE IF NOT EXISTS datalens_partition_marker (
    id BIGSERIAL PRIMARY KEY,
    partitioned BOOLEAN NOT NULL DEFAULT false,
    migrated_at TIMESTAMPTZ,
    note TEXT
);
INSERT INTO datalens_partition_marker (partitioned, note) VALUES (false, 'Partition migration pending. Run: crosslink migrate-partition');
