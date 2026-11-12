-- 审计中心查询索引：username 前缀筛选与 ip 等值筛选（/audit 新筛选参数）。
-- sqlite 链独立编号；沿用本链 CREATE INDEX IF NOT EXISTS 惯例（重复执行幂等）。
CREATE INDEX IF NOT EXISTS idx_audit_logs_username ON audit_logs (username);
CREATE INDEX IF NOT EXISTS idx_audit_logs_ip_address ON audit_logs (ip_address);
