-- ============================================================================
-- 019_extension_catalog: 市场同步缓存（Layer 0 只读快照）
-- ============================================================================
-- 架构角色: 存储市场同步后的目录条目快照。只读缓存，不驱动运行时执行。
--   payload: JSON，含 command/args/env/manifest_url 等类型特定字段。
-- 关联: M13-bis(Extension Registry) §1
-- ============================================================================

CREATE TABLE IF NOT EXISTS extension_catalog (
    id             TEXT PRIMARY KEY,            -- "{publisher}/{name}" slug
    marketplace_id TEXT NOT NULL,               -- plugin_marketplaces.id
    type           TEXT NOT NULL,               -- 'mcp' | 'skill' | 'plugin'
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    publisher      TEXT NOT NULL DEFAULT '',
    trust_tier     INTEGER NOT NULL DEFAULT 1,
    url            TEXT NOT NULL DEFAULT '',
    version        TEXT NOT NULL DEFAULT '',    -- B3: 市场同步得到的最新可用版本号
    payload        TEXT NOT NULL,               -- JSON 全字段快照
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_extension_catalog_mp   ON extension_catalog(marketplace_id);
CREATE INDEX IF NOT EXISTS idx_extension_catalog_type ON extension_catalog(type);

-- ============================================================================
-- extension_catalog_overlay: 目录覆盖层（ADR-0123）
-- ============================================================================
-- 架构角色: 官方插件仓库生成的中文名称/摘要/说明与四维分类（主题/部门/行业/日常生活），
--   按 catalog_id 叠加到 extension_catalog 展示。与 extension_catalog 生命周期不同
--   （后者按市场整体替换），故独立成表；只影响展示，不参与安装决策。
-- 写入: marketplace.OverlaySync 整份替换（事务内先删后写）。
-- 关联: M13-bis §5.6
-- ============================================================================

CREATE TABLE IF NOT EXISTS extension_catalog_overlay (
    catalog_id     TEXT PRIMARY KEY,               -- = extension_catalog.id
    src_hash       TEXT NOT NULL,                  -- 上游 name+description 指纹，不一致即中文过期
    name_zh        TEXT NOT NULL DEFAULT '',
    summary_zh     TEXT NOT NULL DEFAULT '',
    description_zh TEXT NOT NULL DEFAULT '',
    description_en TEXT NOT NULL DEFAULT '',       -- 上游描述为空时由插件清单补出
    topics         TEXT NOT NULL DEFAULT '[]',     -- JSON 数组：主题
    departments    TEXT NOT NULL DEFAULT '[]',     -- JSON 数组：部门
    industries     TEXT NOT NULL DEFAULT '[]',     -- JSON 数组：行业
    life           TEXT NOT NULL DEFAULT '[]',     -- JSON 数组：日常生活
    featured       INTEGER NOT NULL DEFAULT 0,     -- 精选权重，越大越靠前
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- 覆盖层元数据：etag（条件请求）、generated_at、taxonomy（词表 JSON，前端显示名来源）。
CREATE TABLE IF NOT EXISTS extension_catalog_overlay_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- ============================================================================
-- extension_doc_index: 说明文档集中索引（ADR-0123 2026-11-14 复核）
-- ============================================================================
-- 架构角色: 官方插件仓集中生成的 README 说明文档元数据索引（readmes/index.json）。
--   按 catalog_id 关联，记录源文档不可变修订、哈希以及集中中文文档路径与校验哈希。
--   与 extension_catalog_overlay 独立，只影响文档展示，不参与安装决策。
-- 写入: marketplace.DocIndexSync 整份替换。
-- 关联: M13-bis §5.6
-- ============================================================================

CREATE TABLE IF NOT EXISTS extension_doc_index (
    catalog_id          TEXT PRIMARY KEY,
    marketplace_id      TEXT NOT NULL,
    document_key        TEXT NOT NULL,
    source_repo_url     TEXT NOT NULL DEFAULT '',
    source_revision     TEXT NOT NULL DEFAULT '',
    source_path         TEXT NOT NULL DEFAULT '',
    source_kind         TEXT NOT NULL DEFAULT 'readme',
    source_sha256       TEXT NOT NULL DEFAULT '',
    source_base_url     TEXT NOT NULL DEFAULT '',
    catalog_revision    TEXT NOT NULL DEFAULT '',
    zh_path             TEXT NOT NULL DEFAULT '',
    zh_sha256           TEXT NOT NULL DEFAULT '',
    zh_origin           TEXT NOT NULL DEFAULT '',
    zh_review           TEXT NOT NULL DEFAULT '',
    zh_source_sha256    TEXT NOT NULL DEFAULT '',
    generator_version   TEXT NOT NULL DEFAULT '',
    provenance_path     TEXT NOT NULL DEFAULT '',
    rights_id           TEXT NOT NULL DEFAULT '',
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS extension_doc_index_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- ============================================================================
-- extension_doc_translation_tasks: 自定义扩展按需翻译持久任务（State-in-DB）
-- ============================================================================
-- 架构角色: 记录用户自主添加扩展的按需翻译任务状态（queued, translating, ready, failed）。
--   按 (principal_id, catalog_id, source_sha256, locale, generator_version) 幂等去重；
--   正文缓存存储于文件系统受管目录，此处记录状态与缓存索引。
-- 关联: ADR-0123 2026-11-14 复核
-- ============================================================================

CREATE TABLE IF NOT EXISTS extension_doc_translation_tasks (
    task_id             TEXT PRIMARY KEY,
    principal_id        TEXT NOT NULL,
    catalog_id          TEXT NOT NULL,
    source_sha256       TEXT NOT NULL,
    locale              TEXT NOT NULL,
    generator_version   TEXT NOT NULL DEFAULT 'v1',
    status              TEXT NOT NULL DEFAULT 'queued',
    error_code          TEXT,
    retry_after         INTEGER NOT NULL DEFAULT 0,
    attempts            INTEGER NOT NULL DEFAULT 0,
    result_cache_path   TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ext_doc_trans_idempotent
ON extension_doc_translation_tasks(principal_id, catalog_id, source_sha256, locale, generator_version);

CREATE INDEX IF NOT EXISTS idx_ext_doc_trans_status
ON extension_doc_translation_tasks(status);
