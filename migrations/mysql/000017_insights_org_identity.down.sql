-- 降级守护：先建旧四列唯一索引（多组织共存时失败中止，数据不动）。org_id 属于
-- 基线，降级不动它。与 000015 的 down 内容等价且幂等：列/索引删除均带
-- information_schema 探测，down 链 17→…→15 连续回放时第二次执行无副作用。
CREATE UNIQUE INDEX idx_insights_unique_downcheck ON insights(period_key, scope, scope_id, insight_type);
DROP INDEX idx_insights_unique_downcheck ON insights;
-- 探测：现唯一索引存在才删（成对 down 先执行时可能已删/已重建为四列）
SET @ddl = (SELECT IF(COUNT(*) > 0,
    'DROP INDEX idx_insights_unique ON insights',
    'SELECT 1')
FROM information_schema.STATISTICS
WHERE table_schema = DATABASE() AND table_name = 'insights' AND index_name = 'idx_insights_unique');
PREPARE drop_new_unique FROM @ddl;
EXECUTE drop_new_unique;
DEALLOCATE PREPARE drop_new_unique;
-- 探测：org_key 存在才删（成对 down 先执行时可能已删）
SET @ddl = (SELECT IF(COUNT(*) > 0,
    'ALTER TABLE insights DROP COLUMN org_key',
    'SELECT 1')
FROM information_schema.COLUMNS
WHERE table_schema = DATABASE() AND table_name = 'insights' AND column_name = 'org_key');
PREPARE drop_org_key FROM @ddl;
EXECUTE drop_org_key;
DEALLOCATE PREPARE drop_org_key;
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);
