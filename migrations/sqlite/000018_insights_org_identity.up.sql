-- 同 postgres/000094：org_key = COALESCE(org_id,0) 归一身份列 + 五列唯一索引。
-- sqlite 基线（000001）的 insights 已含 org_id（含 idx_insights_org_id），此处只补
-- org_key 生成列并重建唯一索引——不可 ADD COLUMN org_id（全新/升级库 duplicate
-- column 中断整条迁移链，onboarding sqlite 测试抓到过）。SQLite ALTER ADD COLUMN
-- 仅支持 VIRTUAL 生成列，唯一索引可直接引用。
-- 本文件原为商业 overlay/000018，下沉社区链沿用 18 号原位填洞：sqlite runner 按
-- schema_migrations 已记录版本跳过（商业存量库已记录 18，重放安全）；未记录版本
-- 即使小于当前也会应用（社区存量库补洞生效）。
DROP INDEX IF EXISTS idx_insights_unique;
ALTER TABLE insights ADD COLUMN org_key INTEGER GENERATED ALWAYS AS (COALESCE(org_id, 0));
CREATE UNIQUE INDEX idx_insights_unique ON insights(period_key, scope, scope_id, insight_type, org_key);
