-- 洞察行逻辑身份对齐物理唯一性（保留 NULL=全局语义，不改外键、不造组织 0）：
-- 旧唯一键 (period_key, scope, scope_id, insight_type) 不含组织身份，与组织过滤的
-- upsert 不一致（组织上下文遇历史 NULL-org 行 → INSERT 撞唯一索引 → 23505 → 500）。
-- 方案：新增归一身份生成列 org_key = COALESCE(org_id, 0)（NULL 全局行→0，组织行→org id），
-- 唯一索引含 org_key：组织 A / 组织 B / 全局（NULL）三者同周期同作用域共存，互不覆盖。
-- 不回填、不改 org_id 可空性：insights.org_id REFERENCES organizations(id) 外键保持原状。
-- 与 000092 成对：92 负责新装库的顺序连续，本文件负责存量库兜底——已越过 92 的库
-- （版本 ≥93，golang-migrate 静默跳过 92）在此重放同一幂等内容。两文件不可只留其一。
ALTER TABLE insights ADD COLUMN IF NOT EXISTS org_key BIGINT GENERATED ALWAYS AS (COALESCE(org_id, 0)) STORED;
DROP INDEX IF EXISTS idx_insights_unique;
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type, org_key);
