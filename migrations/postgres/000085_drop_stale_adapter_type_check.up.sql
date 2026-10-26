-- Drop the stale adapter_type whitelist: migration 000003 froze a 6-entry
-- list that no longer matches the adapter registry. 'mock' (demo mode seed,
-- 2026-07) and the Chinese-cloud commercial adapters (aliyun_bailian,
-- baidu_qianfan, tencent_hunyuan, volcengine_ark, minimax_image) are all
-- rejected by this constraint today. Adapter validity is enforced at runtime
-- by the provider registry (RegisterAdapter); the DB-level whitelist only
-- guarantees drift.
ALTER TABLE providers DROP CONSTRAINT chk_adapter_type;
