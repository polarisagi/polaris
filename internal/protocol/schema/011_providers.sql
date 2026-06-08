-- ============================================================================
-- 011_providers: LLM 厂商配置（凭据层）+ 模型层
-- ============================================================================
-- 架构角色: 两层架构——providers 管理 API 凭据，provider_models 管理具体模型+角色。
--   凭据层（providers）: 连接参数、认证，不含模型选择
--   模型层（provider_models）: 模型 ID、角色分工（general/default/reasoning）
-- 关联: M1(Inference Runtime), M13(Interface)
-- ============================================================================

CREATE TABLE IF NOT EXISTS providers (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL CHECK(type IN ('openai_compat','anthropic','google_agent_platform','ollama')),
    base_url    TEXT NOT NULL DEFAULT '',
    api_key     TEXT NOT NULL DEFAULT '',
    project_id  TEXT NOT NULL DEFAULT '',
    location    TEXT NOT NULL DEFAULT '',
    sa_key_json TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_providers_type    ON providers(type);
CREATE INDEX IF NOT EXISTS idx_providers_enabled ON providers(enabled);

CREATE TABLE IF NOT EXISTS provider_models (
    id          TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_id    TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT 'general' CHECK(role IN ('general','default','reasoning')),
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_provider_models_provider ON provider_models(provider_id);
CREATE INDEX IF NOT EXISTS idx_provider_models_role     ON provider_models(role);
CREATE INDEX IF NOT EXISTS idx_provider_models_enabled  ON provider_models(enabled);
