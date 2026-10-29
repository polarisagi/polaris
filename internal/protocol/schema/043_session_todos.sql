-- 043_session_todos: 会话级待办清单（State-in-DB，HE-6；ADR-0104 决策三）
-- 架构角色: todo_write / todo_read 内置工具的唯一存储，取代全局 .polaris_todo.json——
--   文件方案下所有会话与子 Agent 共写一份计划，且不在 DB。
-- session_id 取工具执行 ctx 的 CtxTaskIDKey（与 llm_calls.session_id 同源）；刻意不建指向
--   chat_sessions 的外键：headless / 子 Agent 会话没有 chat_sessions 行。会话删除时由
--   SQLiteChatRepository.DeleteSession 同事务清理。
-- seq 为清单内 0 起的顺序号；todo_write 整表替换（单事务先删后插）。
CREATE TABLE IF NOT EXISTS session_todos (
    session_id TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    content    TEXT    NOT NULL,
    status     TEXT    NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','in_progress','completed')),
    updated_at INTEGER NOT NULL,                       -- Unix 毫秒
    PRIMARY KEY(session_id, seq)
) STRICT;
