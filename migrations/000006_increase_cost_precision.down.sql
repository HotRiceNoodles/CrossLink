-- WARNING: Rolling back narrows cost precision from DECIMAL(16,8) to DECIMAL(12,6).
-- Any stored values with more than 6 decimal places will be silently truncated.
-- No data is lost unless cost values actually used more than 6 decimal places.
ALTER TABLE usage_logs ALTER COLUMN cost TYPE DECIMAL(12,6);
