-- Context Engineering Gateway: prompt templates.
-- Stores composable context blocks (system_prompt / few_shot / tool_defs) that the
-- gateway assembles into requests referencing them via the `x_context` field.
-- See docs/plans/2026-07-14-context-engineering-gateway-design.md.
CREATE TABLE prompt_templates (
    id            BIGSERIAL PRIMARY KEY,
    name          VARCHAR(64)  NOT NULL,
    description   VARCHAR(512),
    system_prompt TEXT         NOT NULL DEFAULT '',
    variables_schema JSONB,                 -- [{name,type,required,default,trusted,desc}]
    few_shot      JSONB,                   -- [{role,content}] static, no {{var}} interpolation
    tool_defs     JSONB,                   -- reserved (MVP empty)
    target_format VARCHAR(16)  NOT NULL DEFAULT 'auto',  -- auto|anthropic|openai
    status        SMALLINT     NOT NULL DEFAULT 1,
    version       INT          NOT NULL DEFAULT 1,
    org_id        BIGINT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

-- Partial unique on name (soft-delete aware — same lesson as migration 000067):
-- a soft-deleted template keeps its name but no longer blocks re-creation.
CREATE UNIQUE INDEX prompt_templates_name_key
    ON prompt_templates (name) WHERE deleted_at IS NULL;
CREATE INDEX idx_prompt_templates_deleted_at ON prompt_templates(deleted_at);

-- Track which template assembled each request, for per-template usage analytics.
ALTER TABLE usage_logs ADD COLUMN template_id BIGINT;
CREATE INDEX idx_usage_logs_template_id ON usage_logs(template_id);
