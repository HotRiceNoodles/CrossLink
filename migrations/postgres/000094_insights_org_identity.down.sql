-- 降级守护：多组织同元组共存时，旧四列唯一键无法重建——以明确错误拒绝降级（不破坏数据）。
-- 与 000092 的 down 内容等价且幂等：down 链 94→…→92 连续回放时第二次执行无副作用。
DO $$
DECLARE dup INT;
BEGIN
  SELECT COUNT(*) INTO dup FROM (
    SELECT 1 FROM insights
    GROUP BY period_key, scope, scope_id, insight_type
    HAVING COUNT(DISTINCT COALESCE(org_id, 0)) > 1
  ) t;
  IF dup > 0 THEN
    RAISE EXCEPTION 'cannot downgrade insights org-identity: % tuple(s) exist under multiple org identities; resolve (delete/merge org rows) before downgrade', dup;
  END IF;
END $$;
DROP INDEX IF EXISTS idx_insights_unique;
ALTER TABLE insights DROP COLUMN IF EXISTS org_key;
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type);
