-- 补齐 APIKey 模型的密钥来源和计费倍率字段，覆盖新建库及已有库升级。
-- 历史密钥继续使用原生来源和 1 倍计费。
ALTER TABLE api_keys ADD COLUMN key_source TEXT NOT NULL DEFAULT 'native';
ALTER TABLE api_keys ADD COLUMN source_vendor TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN price_multiplier DECIMAL(6,4) NOT NULL DEFAULT 1.0;
