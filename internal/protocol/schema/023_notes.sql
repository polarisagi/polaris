-- ============================================================================
-- 023_notes: WorkingMemory NotesStore — 跨 Session 轻量笔记持久化
-- ============================================================================
-- 架构角色: M5 §2.2 NotesStore，为 Agent 提供跨 Session 的持久化笔记能力。
-- 写入路径: MutationBus → DatabaseWriter（单写者，CAS Version 乐观锁）
-- 读取路径: M4 S_PERCEIVE 阶段加载关联 Note，m5.WorkingMemory.Notes()
-- 容量约束: 单条 ≤64KB（content 长度），总量按 LRU 淘汰（由 GC 定期清理）
-- TTL 策略: expires_at 字段控制，GC 通过 MutationIntent OpDeleteBatch 提交
-- ============================================================================

CREATE TABLE IF NOT EXISTS notes (
    id          TEXT    PRIMARY KEY,       -- ULID
    key         TEXT    NOT NULL UNIQUE,   -- 用户/Agent 定义的唯一键名
    content     TEXT    NOT NULL,          -- 正文，≤64KB
    version     INTEGER NOT NULL DEFAULT 1, -- CAS 乐观锁版本号，每次 Set 递增
    size_bytes  INTEGER NOT NULL DEFAULT 0, -- content 字节数，用于容量管理
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    expires_at  INTEGER,                   -- Unix 秒，NULL=永不过期，7天默认由应用层设置
    tags_json   TEXT    NOT NULL DEFAULT '[]' -- 标签数组（JSON），便于按主题检索
) STRICT;

CREATE INDEX IF NOT EXISTS idx_notes_key        ON notes(key);
CREATE INDEX IF NOT EXISTS idx_notes_expires    ON notes(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_notes_updated    ON notes(updated_at);
