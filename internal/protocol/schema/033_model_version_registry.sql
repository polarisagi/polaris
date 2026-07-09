-- ============================================================================
-- 033_model_version_registry: ModelVersionRegistry — 模型版本管理注册表
-- ============================================================================
-- 架构角色: 持有各 Provider 模型的版本/废弃状态/兼容性评分，驱动 M1 §9 三档
--           自动迁移策略（>=0.9 自动 / 0.7-0.9 自动+WARN / <0.7 禁止自动）。
-- 与 022_provider_catalog 的区别: sys_provider_models 是"能选哪些模型"的只读
--   字典（用户建 Provider 时用），本表是"这个模型现在健康吗、能不能自动切换
--   继任模型"的运行时状态，两者职责不同、互不替代。
-- 与各 Adapter resolveXXXModel() 的区别: resolve*Model 是编译期硬编码的简化
--   废弃名映射（无兼容性评分），本表是运行时可查询/更新的完整版本状态机；
--   resolve*Model 继续作为无数据库依赖的兜底路径保留，不删除、不改行为。
-- 关联模块: M1(Inference Runtime) §9, M2(Storage Fabric，OnlineReindexer 全量重嵌)
-- ============================================================================

CREATE TABLE IF NOT EXISTS model_version_entries (
    id                  TEXT PRIMARY KEY,
    -- ↑ '{provider}:{model_id}' 复合键，与 sys_provider_models.id 命名约定对齐。

    provider            TEXT NOT NULL,
    model_id            TEXT NOT NULL,
    version             TEXT NOT NULL DEFAULT '',
    -- ↑ 模型自身版本标识（厂商发布的 snapshot/date 版本号，非本表 schema 版本）。

    deprecated          INTEGER NOT NULL DEFAULT 0,
    successor_model_id  TEXT NOT NULL DEFAULT '',
    -- ↑ 废弃后建议的继任模型 model_id（同一 provider 下），三档迁移策略据此切换。

    prompt_template     TEXT NOT NULL DEFAULT '',
    tool_call_style     TEXT NOT NULL DEFAULT '',
    -- ↑ 'native_function_call' | 'react_text' | 'xml_tags' 等，供 Adapter 按需读取。

    max_context         INTEGER NOT NULL DEFAULT 0,
    capabilities        TEXT NOT NULL DEFAULT '{}',
    -- ↑ JSON 对象，如 {"vision":true,"embedding":false,"tool_call":true}。
    --   capabilities.embedding=true 的模型被废弃时触发 M2 OnlineReindexer 全量重嵌。

    validated_on        TEXT NOT NULL DEFAULT '[]',
    -- ↑ JSON 字符串数组，技能兼容测试通过的 skill name 列表（OnModelUpgrade 更新）。

    compatibility_score REAL NOT NULL DEFAULT 1.0,
    -- ↑ 0.0-1.0，OnModelUpgrade 重跑兼容测试后更新；<0.8 触发 WARN。

    consecutive_errors  INTEGER NOT NULL DEFAULT 0,
    -- ↑ 连续 4xx/5xx 调用失败计数；达到 3 触发自动回退到旧模型（RecordCallResult）。

    updated_at          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_model_version_provider
    ON model_version_entries(provider, model_id);

CREATE INDEX IF NOT EXISTS idx_model_version_deprecated
    ON model_version_entries(deprecated) WHERE deprecated = 1;
