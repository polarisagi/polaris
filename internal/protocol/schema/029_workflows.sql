-- ============================================================================
-- 029_workflows: 工作流定义 + 步骤 + 执行历史
-- ============================================================================
-- 架构角色: 多步骤 Agent 任务编排层（Automation 的有序链式扩展）。
-- 与 automations(017) 关系: workflow_steps 可选绑定已有 automation，
--   或直接内联 prompt。每步复用 automations 执行基础设施。
-- 数据交接: 上一步 reply 文本注入为下一步 prompt 前缀（由执行引擎处理）。
-- 电路断路器: 同 automations，连续 5 次 error → circuit_open=1。
-- ============================================================================

CREATE TABLE IF NOT EXISTS workflows (
    id              TEXT    PRIMARY KEY,                 -- "wf_{8字节hex}"
    type            TEXT    NOT NULL DEFAULT 'chain',    -- 'chain' | 'dag'
    name            TEXT    NOT NULL DEFAULT '',
    description     TEXT    NOT NULL DEFAULT '',
    trigger_type    TEXT    NOT NULL DEFAULT 'manual',   -- 'cron' | 'manual'
    cron_schedule   TEXT    NOT NULL DEFAULT '',         -- trigger_type=cron 时有效
    enabled         INTEGER NOT NULL DEFAULT 1,
    -- 执行状态
    last_run_at     TEXT    NOT NULL DEFAULT '',
    next_run_at     TEXT    NOT NULL DEFAULT '',
    run_count       INTEGER NOT NULL DEFAULT 0,
    last_run_status TEXT    NOT NULL DEFAULT '',         -- 'ok' | 'error' | 'running' | ''
    last_run_error  TEXT    NOT NULL DEFAULT '',
    -- 电路断路器（与 automations 同策略）
    failure_count   INTEGER NOT NULL DEFAULT 0,
    circuit_open    INTEGER NOT NULL DEFAULT 0,
    circuit_opened_at TEXT  NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_wf_enabled  ON workflows(enabled);
CREATE INDEX IF NOT EXISTS idx_wf_next_run ON workflows(next_run_at) WHERE next_run_at != '';

-- ─── 步骤 ─────────────────────────────────────────────────────────────────────
-- seq 0-based，保证有序；UNIQUE(workflow_id, seq) 防重排冲突。
-- automation_id 为空时使用内联 prompt；非空时以 automation 配置为准（prompt 忽略）。
-- input_from_prev=1 时，执行引擎将上一步 reply 截断至 2000 字符注入为本步上下文前缀。

CREATE TABLE IF NOT EXISTS workflow_steps (
    id               TEXT    PRIMARY KEY,               -- "ws_{8字节hex}"
    workflow_id      TEXT    NOT NULL,                  -- workflows.id
    seq              INTEGER NOT NULL,                  -- 0-based 执行顺序
    name             TEXT    NOT NULL DEFAULT '',       -- 步骤显示名称
    automation_id    TEXT    NOT NULL DEFAULT '',       -- 可选：绑定已有 automations.id
    prompt           TEXT    NOT NULL DEFAULT '',       -- 内联 prompt（automation_id 为空时使用）
    reasoning_effort TEXT    NOT NULL DEFAULT 'medium', -- 'low' | 'medium' | 'high' | 'ultra'
    working_dir      TEXT    NOT NULL DEFAULT '',
    input_from_prev  INTEGER NOT NULL DEFAULT 1,        -- 是否注入上一步输出
    depends_on       TEXT    NOT NULL DEFAULT '[]',     -- JSON: ["ws_{id}", ...]
    capability_type  TEXT    NOT NULL DEFAULT '',       -- 对应 Agent capability_type
    compensation_tool TEXT   NOT NULL DEFAULT '',       -- 补偿工具名称
    compensation_args TEXT   NOT NULL DEFAULT '',       -- 补偿参数 (JSON)
    UNIQUE(workflow_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_ws_workflow ON workflow_steps(workflow_id, seq);

-- ─── 执行历史 ──────────────────────────────────────────────────────────────────
-- step_outputs: JSON 数组，每步完成后追加 {seq, session_id, status, output_preview}。
-- output_preview 截断至 500 字符供前端列表展示，完整输出通过 session_id 查会话。

CREATE TABLE IF NOT EXISTS workflow_runs (
    id              TEXT    PRIMARY KEY,               -- "wfr_{8字节hex}"
    workflow_id     TEXT    NOT NULL,
    trigger         TEXT    NOT NULL DEFAULT 'manual', -- 'cron' | 'manual'
    status          TEXT    NOT NULL DEFAULT 'running',-- 'running' | 'ok' | 'error' | 'timeout'
    current_step    INTEGER NOT NULL DEFAULT 0,        -- 当前执行步骤 seq（实时更新）
    total_steps     INTEGER NOT NULL DEFAULT 0,
    started_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    finished_at     TEXT    NOT NULL DEFAULT '',
    error_msg       TEXT    NOT NULL DEFAULT '',
    step_outputs    TEXT    NOT NULL DEFAULT '[]'      -- JSON: [{seq,session_id,status,output_preview}]
);

CREATE INDEX IF NOT EXISTS idx_wfr_workflow ON workflow_runs(workflow_id);
CREATE INDEX IF NOT EXISTS idx_wfr_status   ON workflow_runs(status);
CREATE INDEX IF NOT EXISTS idx_wfr_started  ON workflow_runs(started_at DESC);
