-- 同 postgres/000092：org_key = COALESCE(org_id,0) STORED 生成列 + 五列唯一索引。
-- mysql 基线（000001）的 insights 已含 org_id，此处只补 org_key 并重建唯一索引。
-- 本槽位曾由商业 overlay 使用（商业存量库已应用并记录 15），现回填与 000017 内容
-- 等价的幂等迁移：新装库经 15 顺序应用；已越过 15 的存量库（golang-migrate 只向前
-- 应用、静默跳过本文件）由 000017 兜底重放，两路收敛同一形态。15/17 成对存在，
-- 不可只留其一。列添加与旧索引删除均以 information_schema 探测 + PREPARE 守护
-- （MySQL 8.0 无 ADD COLUMN IF NOT EXISTS），任意库况重放安全。
-- 探测：org_key 缺失才补列（已建则跳过，避免 duplicate column 中断迁移链）
SET @ddl = (SELECT IF(COUNT(*) = 0,
    'ALTER TABLE insights ADD COLUMN org_key BIGINT GENERATED ALWAYS AS (COALESCE(org_id, 0)) STORED',
    'SELECT 1')
FROM information_schema.COLUMNS
WHERE table_schema = DATABASE() AND table_name = 'insights' AND column_name = 'org_key');
PREPARE add_org_key FROM @ddl;
EXECUTE add_org_key;
DEALLOCATE PREPARE add_org_key;
-- 探测：旧唯一索引存在才删（部分失败续跑时可能已删）
SET @ddl = (SELECT IF(COUNT(*) > 0,
    'DROP INDEX idx_insights_unique ON insights',
    'SELECT 1')
FROM information_schema.STATISTICS
WHERE table_schema = DATABASE() AND table_name = 'insights' AND index_name = 'idx_insights_unique');
PREPARE drop_old_unique FROM @ddl;
EXECUTE drop_old_unique;
DEALLOCATE PREPARE drop_old_unique;
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type, org_key);
