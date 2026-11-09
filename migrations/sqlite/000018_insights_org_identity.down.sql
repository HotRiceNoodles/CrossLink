-- 降级守护：先尝试重建旧四列唯一索引（多组织共存时此处即失败、整体中止，数据不动），
-- 成功才切换。避免先 DROP 后 CREATE 失败导致无索引状态。org_id 属于基线，降级不动它。
CREATE UNIQUE INDEX idx_insights_unique_downcheck ON insights(period_key, scope, scope_id, insight_type);
DROP INDEX idx_insights_unique_downcheck;
DROP INDEX IF EXISTS idx_insights_unique;
ALTER TABLE insights DROP COLUMN org_key;
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);
