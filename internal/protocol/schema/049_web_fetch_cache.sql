-- ============================================================================
-- 049_web_fetch_cache: web_fetch / feed_fetch 条件请求缓存（ADR-0114 决策三）
-- ============================================================================
-- 架构角色: 让重复抓取同一 URL 走 If-None-Match / If-Modified-Since；内容未变时
--   web_fetch 直接回缓存 Markdown 并标 unchanged=true，管线据此跳过下游（省 token）。
-- markdown 只存抽取结果（≤256KiB），feed_fetch 行 markdown 为空串。
-- 容量: 行数超阈值 m7_tool.web_fetch_cache_max_rows 时按 fetched_at 删最旧。
-- ============================================================================
CREATE TABLE IF NOT EXISTS web_fetch_cache (
    url_hash      TEXT PRIMARY KEY,             -- sha256(url) hex
    url           TEXT NOT NULL,
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    content_hash  TEXT NOT NULL DEFAULT '',     -- sha256(抽取后全文) 前 16 hex
    title         TEXT NOT NULL DEFAULT '',
    markdown      TEXT NOT NULL DEFAULT '',
    fetched_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_wfc_fetched ON web_fetch_cache(fetched_at);
