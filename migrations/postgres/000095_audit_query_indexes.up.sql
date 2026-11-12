-- 审计中心查询索引：username 前缀筛选与 ip 等值筛选（/audit 新筛选参数）。
-- 写入为批 50 异步 flush，两个新索引对写路径影响可忽略；action IN 复用现有
-- action 单列索引。各方言独立编号（postgres 链），mysql/sqlite 同形迁移各自编号。
CREATE INDEX idx_audit_logs_username ON audit_logs (username);
CREATE INDEX idx_audit_logs_ip_address ON audit_logs (ip_address);
