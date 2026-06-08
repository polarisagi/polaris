-- ============================================================================
-- 013_chat: Web UI 对话历史 + FTS5 全文检索
-- ============================================================================

CREATE TABLE IF NOT EXISTS chat_sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS chat_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT    NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role       TEXT    NOT NULL CHECK(role IN ('user','assistant','system')),
    content    TEXT    NOT NULL,
    tool_calls TEXT,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_chat_msg_session ON chat_messages(session_id, id);

-- FTS5 全文检索（content= 模式，实体内容读取走 chat_messages）
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content=chat_messages,
    content_rowid=id,
    tokenize='unicode61'
);

-- 填充已有数据
INSERT OR IGNORE INTO messages_fts(rowid, content)
SELECT id, content FROM chat_messages WHERE role IN ('user','assistant');

CREATE TRIGGER IF NOT EXISTS fts_insert AFTER INSERT ON chat_messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS fts_delete AFTER DELETE ON chat_messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
    VALUES ('delete', old.id, old.content);
END;

CREATE TRIGGER IF NOT EXISTS fts_update AFTER UPDATE ON chat_messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
    VALUES ('delete', old.id, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
