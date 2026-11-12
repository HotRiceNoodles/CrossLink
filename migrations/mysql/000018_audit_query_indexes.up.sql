-- 审计中心查询索引：username 前缀筛选与 ip 等值筛选（/audit 新筛选参数）。
-- mysql 链独立编号（audit_logs 表由 000001_init_schema 建立）；与 postgres
-- 000095 / sqlite 000021 同形。
CREATE INDEX idx_audit_logs_username ON audit_logs (username);
CREATE INDEX idx_audit_logs_ip_address ON audit_logs (ip_address);
