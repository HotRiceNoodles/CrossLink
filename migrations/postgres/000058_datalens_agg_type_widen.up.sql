-- Widen agg_type to accommodate longer values like 'daily_distinct' (15 chars).
ALTER TABLE datalens_agg_status ALTER COLUMN agg_type TYPE VARCHAR(32);
