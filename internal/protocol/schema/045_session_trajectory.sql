-- 045_session_trajectory: 会话轨迹账本（State-in-DB，HE-6；ADR-0104 决策七）
-- 架构角色: cmd/polaris/boot_events.go storeEventWriter 是唯一写入点，取代 KV `events:session:{sid}:{ts}_{seq}`。
--   一行一事件（状态迁移 / LLM 调用 / 工具调用）；工具事件带 tool_name/tool_ok/latency_ms 列，
--   使 PolicyEvolver 与 Markov 矩阵 warm-start、任务画布渲染都能直接按列查询，无需反序列化 payload。
--   seq 会话内单调从 1 起，由 `INSERT … SELECT COALESCE(MAX(seq),0)+1` 单语句分配（写连接单写者保证原子），
--   同时是崩溃恢复回放与 RunReplay 连续性检查的排序/缺口判据（取代纳秒时间戳 + 进程级全局计数器）。
-- 与 events 表（MutationBus 哈希链 EventLog）刻意不合并：两者读写模式与完整性要求不同（ADR-0104 被驳回方案）。
-- event_type: llm_call / tool_call / 状态迁移名（fmt.Sprintf("%d", State)）；tool_ok 仅工具事件（0/1）。
-- payload: 原 KV 值的 JSON（llm_call: request/response；tool_call: tool/args/result）。
CREATE TABLE IF NOT EXISTS session_trajectory (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT    NOT NULL,
    seq         INTEGER NOT NULL,                              -- 会话内单调，从 1 起
    event_type  TEXT    NOT NULL,
    tool_name   TEXT,                                          -- 仅工具事件
    tool_ok     INTEGER CHECK(tool_ok IS NULL OR tool_ok IN (0,1)),
    latency_ms  INTEGER,                                       -- 仅工具事件
    payload     TEXT    NOT NULL,                              -- JSON
    created_at  INTEGER NOT NULL,                              -- Unix 毫秒
    UNIQUE(session_id, seq)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_session_trajectory_tool ON session_trajectory(tool_name, id) WHERE tool_name IS NOT NULL;
