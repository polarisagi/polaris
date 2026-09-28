-- ============================================================================
-- 013_chat: Web UI 对话历史 + FTS5 全文检索
-- ============================================================================

-- projects: 会话的运行上下文容器（ADR-0097）。
-- root_path 为空串 = 无目录项目（纯聊天）；非空时存**规范路径**（Abs + EvalSymlinks，
-- 写入侧保证），信任判定依赖此规范化，禁止存用户输入的原始路径。
-- trusted 仅表示用户对 root_path 内 AGENTS.md/CLAUDE.md 的显式信任（ADR-0088 决策三），
-- 默认 0；instructions 是用户自撰项目指令（写入面限本地可信客户端，见 ADR-0097 决策二）。
-- 'default' 项目恒存在：存量会话 / channel 会话 / 无头会话一律归属它，不可删除。
CREATE TABLE IF NOT EXISTS projects (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    root_path    TEXT NOT NULL DEFAULT '',
    instructions TEXT NOT NULL DEFAULT '',
    trusted      INTEGER NOT NULL DEFAULT 0 CHECK(trusted IN (0,1)),
    archived     INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)),
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

INSERT OR IGNORE INTO projects(id, name) VALUES('default', '默认项目');

CREATE TABLE IF NOT EXISTS chat_sessions (
    id              TEXT PRIMARY KEY,
    title           TEXT NOT NULL DEFAULT '',
    project_id      TEXT NOT NULL DEFAULT 'default' REFERENCES projects(id),
    thrashing_index REAL NOT NULL DEFAULT 0.0,
    -- app_model_context: MCP Apps（apps_spec.mdx）View 模型上下文更新按
    -- server_id 为键的最新一份未消费内容，JSON 对象 {server_id: {content?, structuredContent?}}。
    -- 规范要求"只把最后一次更新送给模型"——每个 (session, server) 只保留最新一份，
    -- 下一轮提示词装配注入后清空该 server_id 对应键（M8f-1）。
    app_model_context TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS chat_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role              TEXT    NOT NULL CHECK(role IN ('user','assistant','system')),
    content           TEXT    NOT NULL,
    reasoning_content TEXT    NOT NULL DEFAULT '',
    tool_calls        TEXT,
    file_offset INTEGER NOT NULL DEFAULT 0,
    file_length INTEGER NOT NULL DEFAULT 0,
    -- dedupe_key: SaveMessage 每次调用生成的稳定幂等键（GD-13-004 复核修复）。
    -- 直接同步写入失败重试耗尽后，会通过 outbox 异步兜底重投（见
    -- TopicChatMessagePersistRetry / ChatMessagePersistHandler），OutboxWorker
    -- 按 at-least-once 语义可能多次调用该 handler；dedupe_key 唯一索引保证
    -- AppendMessageIdempotent 的 INSERT OR IGNORE 不会因重投产生重复消息行。
    -- 允许 NULL（历史行/未走该路径的行），SQLite UNIQUE 索引视多个 NULL 互不冲突。
    dedupe_key  TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_chat_sessions_project ON chat_sessions(project_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_chat_msg_session ON chat_messages(session_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_msg_dedupe_key ON chat_messages(dedupe_key) WHERE dedupe_key IS NOT NULL;

-- chat_app_views: MCP Apps（io.modelcontextprotocol/ui）工具调用产生的 UI 视图
-- 快照（M8f-1，HE-6 State-in-DB）。view_id 由网关侧 crypto/rand 生成
-- （types.NewToolUIViewID）。message_id 关联到触发本次视图的 assistant 消息——
-- tool_ui 事件产生于 FSM 工具执行期，早于本轮 assistant 消息落库（chat_messages.id
-- 是 AUTOINCREMENT，写入前不可知），故允许为 NULL，由 session 编排层在 assistant
-- 消息落库后用 LinkAppViewsToMessage 回填（最稳定的现成关联键：真实 chat_messages
-- 主键，而非另造一个弱关联字段）。widget_state 是 ChatGPT window.openai.widgetState
-- 兼容持久化（PUT /v1/mcp-apps/views/{id}/state）。
CREATE TABLE IF NOT EXISTS chat_app_views (
    view_id      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    message_id   INTEGER REFERENCES chat_messages(id) ON DELETE CASCADE,
    server_id    TEXT NOT NULL,
    resource_uri TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    tool_input   TEXT NOT NULL DEFAULT '{}',
    tool_result  TEXT NOT NULL DEFAULT '{}',
    widget_state TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_chat_app_views_session ON chat_app_views(session_id);
CREATE INDEX IF NOT EXISTS idx_chat_app_views_message ON chat_app_views(message_id);

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
