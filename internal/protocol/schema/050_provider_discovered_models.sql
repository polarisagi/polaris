-- ============================================================================
-- 050_provider_discovered_models: 实时探测发现的厂商模型目录（ADR-0115 WP3）
-- ============================================================================
-- 架构角色: 记录各启用厂商实时探测发现的模型清单及能力特征，供模型自选与目录聚合使用。
-- 关联: M1(Inference Runtime), M13(Interface)
-- ============================================================================

CREATE TABLE IF NOT EXISTS provider_discovered_models (
    provider_id           TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_id              TEXT NOT NULL,
    display_name          TEXT,
    context_window        INTEGER,
    max_output            INTEGER,
    supports_thinking     INTEGER, -- 0/1, 未知存 NULL
    supports_vision       INTEGER, -- 0/1, 未知存 NULL
    supports_tools        INTEGER, -- 0/1, 未知存 NULL
    input_price_per_mtok  REAL,    -- 未知存 NULL
    output_price_per_mtok REAL,    -- 未知存 NULL
    raw_json              TEXT,
    discovered_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    last_seen_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    PRIMARY KEY (provider_id, model_id)
);

CREATE INDEX IF NOT EXISTS idx_discovered_models_provider ON provider_discovered_models(provider_id);
