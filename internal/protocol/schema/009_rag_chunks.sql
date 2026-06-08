-- ============================================================================
-- 009_rag_chunks: 知识库文档块 + FTS5 全文检索
-- ============================================================================
-- 架构角色: M10 Knowledge RAG 的文档存储层。每 chunk 携带 lineage metadata
--           保证溯源完整性（inv_M10_03）。
-- 关联: M10(Knowledge RAG)
-- ============================================================================

CREATE TABLE IF NOT EXISTS rag_chunks (
    id                 TEXT    PRIMARY KEY,
    doc_id             TEXT    NOT NULL,
    content            TEXT    NOT NULL,
    taint_level        INTEGER NOT NULL DEFAULT 1,
    taint_source       TEXT,
    -- lineage metadata（inv_M10_03）
    source_uri         TEXT    NOT NULL DEFAULT '',
    doc_version        TEXT    NOT NULL DEFAULT '',
    chunk_seq          INTEGER NOT NULL DEFAULT 0,
    content_hash       TEXT    NOT NULL DEFAULT '',
    -- 向量嵌入版本（inv_M5_03）：空字符串=未索引，OnlineReindexer 触发条件
    embed_model_version TEXT   NOT NULL DEFAULT '',
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_rag_doc       ON rag_chunks(doc_id);
CREATE INDEX IF NOT EXISTS idx_rag_embed_ver ON rag_chunks(embed_model_version)
    WHERE embed_model_version = '';

-- FTS5 全文检索（content= 模式，实际内容读取走 rag_chunks）
CREATE VIRTUAL TABLE IF NOT EXISTS rag_chunks_fts USING fts5(
    content,
    content='rag_chunks',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS rag_chunks_ai AFTER INSERT ON rag_chunks BEGIN
    INSERT INTO rag_chunks_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER IF NOT EXISTS rag_chunks_ad AFTER DELETE ON rag_chunks BEGIN
    INSERT INTO rag_chunks_fts(rag_chunks_fts, rowid, content)
    VALUES ('delete', old.rowid, old.content);
END;

CREATE TRIGGER IF NOT EXISTS rag_chunks_au AFTER UPDATE ON rag_chunks BEGIN
    INSERT INTO rag_chunks_fts(rag_chunks_fts, rowid, content)
    VALUES ('delete', old.rowid, old.content);
    INSERT INTO rag_chunks_fts(rowid, content) VALUES (new.rowid, new.content);
END;
