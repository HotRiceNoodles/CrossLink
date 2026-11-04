-- M3b 工具维度日聚合表补齐 mysql 链（pg 000089+000091 合并建表，含 disappeared_count）。
-- 表结构与其余 datalens 预聚合表一样跨方言存在：U-11 的 PG-only 指聚合 SQL
-- （$N 占位符/表达式 ON CONFLICT 目标），但表缺缺会让 mysql 的 DataLens 工具
-- 查询直接 SQL 报错。PG 的表达式唯一索引（org_id, COALESCE(api_key_id,-1), …）
-- 在 MySQL 用 STORED 生成列 api_key_norm 等价实现。
CREATE TABLE datalens_tool_daily_metrics (
    id                       BIGINT AUTO_INCREMENT PRIMARY KEY,
    org_id                   BIGINT NOT NULL,
    day_bucket               DATE NOT NULL,
    api_key_id               BIGINT NULL,
    api_key_norm             BIGINT GENERATED ALWAYS AS (IFNULL(api_key_id, -1)) STORED,
    tool_name                VARCHAR(256) NOT NULL,
    mcp_calls                BIGINT NOT NULL DEFAULT 0,
    mcp_errors               BIGINT NOT NULL DEFAULT 0,
    mcp_blocked              BIGINT NOT NULL DEFAULT 0,
    total_duration_ms        BIGINT NOT NULL DEFAULT 0,
    duration_samples         BIGINT NOT NULL DEFAULT 0,
    first_seen_count         INT NOT NULL DEFAULT 0,
    definition_changed_count INT NOT NULL DEFAULT 0,
    disappeared_count        INT NOT NULL DEFAULT 0,
    created_at               DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY idx_dtdm_upsert (org_id, api_key_norm, day_bucket, tool_name),
    INDEX idx_dtdm_org_day (org_id, day_bucket),
    INDEX idx_dtdm_tool_day (org_id, tool_name, day_bucket)
);
