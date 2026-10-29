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
