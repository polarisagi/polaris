-- ============================================================================
-- 029_workflows: 工作流定义 + 步骤 + 执行历史 + 管线数据与跨运行去重
-- ============================================================================
-- 架构角色: 多步骤 Agent / Pipeline 任务编排层（ADR-0114 信息采集与自动化管线）。
-- 步骤类型（kind）: 'agent'（大模型会话）| 'tool'（免 Agent 工具调用）|
--   'transform'（数据算子变换）| 'llm'（单次纯推理总结）。
-- config_json: 当 kind≠agent 时承载 pipeline.StepSpec 完整 JSON 配置。
-- run_data: 状态落地于 workflow_run_data 表，实现步骤间数据交接与历史查看。
-- seen_items: 跨周期去重键记录于 workflow_seen_items 表，成功收尾后提交。
-- 与 automations(017) 关系: workflow 可通过 trigger_type=cron 独立调度，
--   或由 automations 触发；result_action 支持 session/silent/channel 统一投递。
-- 电路断路器: 连续 5 次 error → circuit_open=1。
-- ============================================================================

CREATE TABLE IF NOT EXISTS workflows (
    id              TEXT    PRIMARY KEY,                 -- "wf_{8字节hex}"
    type            TEXT    NOT NULL DEFAULT 'chain',    -- 'chain' | 'dag'
    name            TEXT    NOT NULL DEFAULT '',
    description     TEXT    NOT NULL DEFAULT '',
    trigger_type    TEXT    NOT NULL DEFAULT 'manual',   -- 'cron' | 'manual'
    cron_schedule   TEXT    NOT NULL DEFAULT '',         -- trigger_type=cron 时有效
    enabled         INTEGER NOT NULL DEFAULT 1,
    result_action   TEXT    NOT NULL DEFAULT 'session',  -- 'session' | 'silent' | 'channel:{id}'（同 automations）
    deliver_from    TEXT    NOT NULL DEFAULT '',         -- 投递来源步骤 step_key；空=最后一个未跳过的 llm/agent 步骤
    source          TEXT    NOT NULL DEFAULT 'user',     -- 'user' | 'agent' | 'plugin:<id>'（ADR-0114）
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
-- 2026-07-12：depends_on/capability_type/max_retries 接入 StateGraphExecutor（编排
-- 模式10）执行引擎（workflow_graph.go buildGraphSpec）：仅 workflows.type='dag' 时
-- 生效——depends_on 非空构造真实 DAG 并行边（AND-Join，等待全部声明前驱完成）；
-- depends_on 为空是真正的并行入口。type='chain'（默认）时完全忽略 depends_on，
-- 按 seq 合成顺序链（向后兼容既有语义）。depends_on 的 JSON 数组元素是**0-based
-- seq 索引的字符串**（如 '["0","2"]'），不是步骤 id——因为 CreateWorkflowWithSteps/
-- UpdateWorkflowWithSteps 每次保存都会为全部步骤重新生成 id（整表先删后插），id
-- 在两次保存之间不稳定，无法作为前端可持久引用的依赖锚点，seq 索引才是稳定引用。
-- max_retries>0 的步骤额外附加自环条件边实现失败重试。compensation_* 与
-- MaxVisits>1（即 max_retries>0）互斥（StateGraphExecutor 校验阶段拒绝，Saga
-- 逆序补偿语义在多次执行节点上未定义），故同一步骤不得同时配置两者。

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
    max_retries      INTEGER NOT NULL DEFAULT 0,        -- 失败重试次数（StateGraphExecutor 自环边 MaxVisits=1+此值）
    step_key         TEXT    NOT NULL DEFAULT '',        -- 管线内稳定引用名（模板 {{steps.<key>.*}}）；agent 旧步骤可空
    kind             TEXT    NOT NULL DEFAULT 'agent' CHECK(kind IN ('agent','tool','transform','llm')),
    config_json      TEXT    NOT NULL DEFAULT '{}',      -- kind≠agent 时为 pipeline.StepSpec 完整 JSON
    UNIQUE(workflow_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_ws_workflow ON workflow_steps(workflow_id, seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ws_key ON workflow_steps(workflow_id, step_key) WHERE step_key != '';

-- ─── 执行历史 ──────────────────────────────────────────────────────────────────
-- step_outputs: JSON 数组，每步完成后追加 {seq, session_id, status, output_preview}。
-- output_preview 截断至 500 字符供前端列表展示，完整输出通过 session_id 查会话。

CREATE TABLE IF NOT EXISTS workflow_runs (
    id              TEXT    PRIMARY KEY,               -- "wfr_{8字节hex}"
    workflow_id     TEXT    NOT NULL,
    trigger         TEXT    NOT NULL DEFAULT 'manual', -- 'cron' | 'manual'
    status          TEXT    NOT NULL DEFAULT 'running',-- 'running' | 'ok' | 'error' | 'timeout' | 'interrupted'
    current_step    INTEGER NOT NULL DEFAULT 0,        -- 当前执行步骤 seq（实时更新）
    total_steps     INTEGER NOT NULL DEFAULT 0,
    started_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    finished_at     TEXT    NOT NULL DEFAULT '',
    error_msg       TEXT    NOT NULL DEFAULT '',
    step_outputs    TEXT    NOT NULL DEFAULT '[]',     -- JSON: [{seq,session_id,status,output_preview}]
    skipped         INTEGER NOT NULL DEFAULT 0         -- 1=无新数据整次跳过（不投递、不调 LLM）
);

CREATE INDEX IF NOT EXISTS idx_wfr_workflow ON workflow_runs(workflow_id);
CREATE INDEX IF NOT EXISTS idx_wfr_status   ON workflow_runs(status);
CREATE INDEX IF NOT EXISTS idx_wfr_started  ON workflow_runs(started_at DESC);

-- ─── 步骤数据（State-in-DB）─────────────────────────────────────────────────────
-- 每步归一化输出，供下游模板引用与前端查看；每工作流保留最近 N 次运行
CREATE TABLE IF NOT EXISTS workflow_run_data (
    run_id      TEXT    NOT NULL,
    step_key    TEXT    NOT NULL,
    status      TEXT    NOT NULL,                 -- 'ok' | 'error' | 'skipped'
    data_json   TEXT    NOT NULL DEFAULT '{}',    -- {"items":[...],"text":"...","count":N,"truncated":bool}
    item_count  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    PRIMARY KEY (run_id, step_key)
);

-- ─── 跨运行去重（Cross-run Dedup）────────────────────────────────────────────────
-- dedup scope=workflow 的已见条目；只在运行成功收尾后写入
CREATE TABLE IF NOT EXISTS workflow_seen_items (
    workflow_id   TEXT NOT NULL,
    item_key      TEXT NOT NULL,                  -- sha256(dedup 字段值) 前 32 hex
    first_seen_at TEXT NOT NULL,
    PRIMARY KEY (workflow_id, item_key)
);
CREATE INDEX IF NOT EXISTS idx_wsi_seen ON workflow_seen_items(first_seen_at);

