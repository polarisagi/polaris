-- 042_subagent_runs: 子 Agent 运行记录（State-in-DB，HE-6；ADR-0104 决策六）
-- 架构角色: orchestrator.SubagentRunner 是唯一写入点，三条入口（委派任务 / context: fork 技能 /
--   agent 类型 hook）共用；记录父子血缘、失败原因与 SubagentStop 续跑次数。
--   纯观测性账本，写失败不影响子 Agent 执行；进程崩溃遗留的 running 行由启动期孤儿对账
--   （cmd/polaris/boot_orphan_reconciler.go）置为 interrupted。
-- child_session_id = "sub-"+id，子 Agent headless 运行所用的确定会话 ID，用于关联轨迹表。
CREATE TABLE IF NOT EXISTS subagent_runs (
    id                TEXT    PRIMARY KEY,                 -- = SubagentRequest.AgentID
    parent_session_id TEXT    NOT NULL DEFAULT '',
    child_session_id  TEXT    NOT NULL DEFAULT '',
    task_id           TEXT,                                -- 仅委派入口有值（tasks.task_id）
    agent_type        TEXT    NOT NULL DEFAULT '',
    entry             TEXT    NOT NULL CHECK(entry IN ('delegation','fork_skill','hook')),
    status            TEXT    NOT NULL CHECK(status IN ('running','ok','error','interrupted')),
    prompt            TEXT    NOT NULL DEFAULT '',         -- 截断至 4000 字符
    output            TEXT    NOT NULL DEFAULT '',         -- 截断至 8000 字符
    error             TEXT    NOT NULL DEFAULT '',
    continuations     INTEGER NOT NULL DEFAULT 0,          -- SubagentStop hook 续跑次数
    started_at        INTEGER NOT NULL,                    -- Unix 毫秒
    finished_at       INTEGER                              -- Unix 毫秒
) STRICT;

CREATE INDEX IF NOT EXISTS idx_subagent_runs_parent ON subagent_runs(parent_session_id, started_at);
CREATE INDEX IF NOT EXISTS idx_subagent_runs_status ON subagent_runs(status);
