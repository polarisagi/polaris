-- ============================================================================
-- 054_messages_trigram_fts: 对话消息的 trigram 全文索引（ADR-0120 决策六）
-- ============================================================================
-- 架构角色: session_search 工具的检索底座。013 的 messages_fts 使用 unicode61 分词，
--           对中文整句无法切词；trigram 对任意语言的 ≥3 字符子串都能命中。
--           ≤2 字符的 CJK 查询由应用层回退为 LIKE 有界扫描。
-- 关联: M05(Memory)、ADR-0120
-- ============================================================================

CREATE VIRTUAL TABLE IF NOT EXISTS messages_trigram_fts USING fts5(
    content,
    content=chat_messages,
    content_rowid=id,
    tokenize='trigram'
);

-- 回填已有消息（仅用户/助手可检索内容）
INSERT INTO messages_trigram_fts(rowid, content)
SELECT id, content FROM chat_messages WHERE role IN ('user','assistant');

CREATE TRIGGER IF NOT EXISTS trg_fts_insert AFTER INSERT ON chat_messages BEGIN
    INSERT INTO messages_trigram_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS trg_fts_delete AFTER DELETE ON chat_messages BEGIN
    INSERT INTO messages_trigram_fts(messages_trigram_fts, rowid, content)
    VALUES ('delete', old.id, old.content);
END;

CREATE TRIGGER IF NOT EXISTS trg_fts_update AFTER UPDATE OF content ON chat_messages BEGIN
    INSERT INTO messages_trigram_fts(messages_trigram_fts, rowid, content)
    VALUES ('delete', old.id, old.content);
    INSERT INTO messages_trigram_fts(rowid, content) VALUES (new.id, new.content);
END;
