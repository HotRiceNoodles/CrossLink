ALTER TABLE api_keys ADD COLUMN max_calls int NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN call_period varchar(16) NOT NULL DEFAULT 'daily';
