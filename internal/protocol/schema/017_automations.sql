-- ============================================================================
-- 017_automations: 自动化任务 + 执行历史
-- ============================================================================
-- 架构角色: 定时/触发式 Agent 任务的配置层。三层触发：cron | webhook | manual。
-- 与 channels(012)关联：webhook 触发复用已有渠道基础设施。
-- 与 chat_sessions(013)关联：每次执行产生独立 session（带 automation_id 标签）。
-- result_action: session=追加到自动 session | channel:{id}=渠道推送 | silent=静默
-- trigger_type: cron=定时 | webhook=外部事件 | both=两者 | manual=仅手动
-- working_dir: 可选文件操作根路径（任意目录；留空则 Agent 自主决定是否操作文件）
-- reasoning_effort: low/medium/high/ultra 自动映射 model_roles（用户不选模型）
-- ============================================================================

CREATE TABLE IF NOT EXISTS automations (
    id                TEXT    PRIMARY KEY,               -- "auto_{8字节hex}"
    name              TEXT    NOT NULL DEFAULT '',
    prompt            TEXT    NOT NULL,
    trigger_type      TEXT    NOT NULL DEFAULT 'cron',  -- 'cron' | 'webhook' | 'both' | 'manual'
    cron_schedule     TEXT    NOT NULL DEFAULT '',       -- cron 表达式；trigger_type=webhook 时可空
    channel_id        TEXT    NOT NULL DEFAULT '',       -- channels.id；webhook 触发时非空
    working_dir       TEXT    NOT NULL DEFAULT '',       -- 可选文件操作根路径；留空=Agent 自主决定
    reasoning_effort  TEXT    NOT NULL DEFAULT 'medium', -- 'low' | 'medium' | 'high' | 'ultra'
    result_action     TEXT    NOT NULL DEFAULT 'session',-- 'session' | 'channel:{id}' | 'silent'
    sandbox_level     INTEGER NOT NULL DEFAULT 2,        -- Sandbox-L2（Wasm，默认）
    cedar_rules_json  TEXT    NOT NULL DEFAULT '[]',     -- JSON Cedar 显式授权规则
    enabled           INTEGER NOT NULL DEFAULT 1,
    -- 执行状态（cronTick 更新）
    last_run_at       TEXT    NOT NULL DEFAULT '',
    next_run_at       TEXT    NOT NULL DEFAULT '',       -- cronTick 预计算下次触发时间
    run_count         INTEGER NOT NULL DEFAULT 0,
    last_run_status   TEXT    NOT NULL DEFAULT '',       -- 'ok' | 'error' | 'running' | ''
    last_run_error    TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_auto_enabled     ON automations(enabled);
CREATE INDEX IF NOT EXISTS idx_auto_trigger     ON automations(trigger_type);
CREATE INDEX IF NOT EXISTS idx_auto_next_run    ON automations(next_run_at) WHERE next_run_at != '';
CREATE INDEX IF NOT EXISTS idx_auto_channel     ON automations(channel_id)  WHERE channel_id != '';

-- ─── 执行历史 ──────────────────────────────────────────────────────────────
-- 每次触发（定时/webhook/手动）产生一条 run 记录。
-- session_id 指向 chat_sessions(013)，可跳入对应会话查看执行过程。

CREATE TABLE IF NOT EXISTS automation_runs (
    id             TEXT    PRIMARY KEY,               -- "run_{8字节hex}"
    automation_id  TEXT    NOT NULL,                  -- automations.id
    trigger        TEXT    NOT NULL DEFAULT 'cron',   -- 'cron' | 'webhook' | 'manual'
    status         TEXT    NOT NULL DEFAULT 'running',-- 'running' | 'ok' | 'error' | 'timeout'
    session_id     TEXT    NOT NULL DEFAULT '',       -- chat_sessions.id；执行产生的 session
    started_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    finished_at    TEXT    NOT NULL DEFAULT '',
    error_msg      TEXT    NOT NULL DEFAULT '',
    prompt_snapshot TEXT   NOT NULL DEFAULT ''        -- 执行时的 prompt 快照（prompt 可能被修改）
);

CREATE INDEX IF NOT EXISTS idx_runs_automation ON automation_runs(automation_id);
CREATE INDEX IF NOT EXISTS idx_runs_status     ON automation_runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_started    ON automation_runs(started_at DESC);
