-- 039_llm_calls.sql
-- 每次 LLM Provider 调用一行（ADR-0101 决策六）：谁（用途/会话）调用了哪个模型、花了多少 token 与钱。
-- 写入方：internal/llm 注册表对每个 Provider 的记录包装（覆盖路由主路径、failover、降级池、
-- 溢出升级与 PickProvider 直取），经 internal/store/repo.SQLiteLLMCallRepository 异步落库。
CREATE TABLE IF NOT EXISTS llm_calls (
    id               TEXT PRIMARY KEY,
    created_at       INTEGER NOT NULL,             -- Unix 毫秒，调用发起时刻
    session_id       TEXT    NOT NULL DEFAULT '',  -- ctx 中的任务/会话作用域（CtxTaskIDKey），后台调用可为空
    purpose          TEXT    NOT NULL DEFAULT '',  -- InferOptions.Purpose：perceive/plan/reflect/respond/consolidate_summary/...
    provider         TEXT    NOT NULL,             -- 注册名（providers 表 name）
    model_id         TEXT    NOT NULL DEFAULT '',  -- 实际模型 ID（请求显式指定优先，否则 Provider.ModelID()）
    model_pool       TEXT    NOT NULL DEFAULT '',  -- 请求的 Model Pool（default/general/reasoning/budget）
    thinking_mode    TEXT    NOT NULL DEFAULT '',  -- 下发的思考档位，空 = Provider 默认
    streaming        INTEGER NOT NULL DEFAULT 0,
    status           TEXT    NOT NULL,             -- ok | error | cancelled
    input_tokens     INTEGER NOT NULL DEFAULT 0,   -- 全部输入 token（含缓存命中）
    cache_hit_tokens INTEGER NOT NULL DEFAULT 0,   -- 其中命中前缀缓存的部分
    output_tokens    INTEGER NOT NULL DEFAULT 0,   -- 全部输出 token（含推理）
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,   -- 其中推理 token
    latency_ms       INTEGER NOT NULL DEFAULT 0,
    cost_usd         REAL    NOT NULL DEFAULT 0,   -- 按 ProviderCapabilities 费率估算
    error            TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_llm_calls_created_at ON llm_calls(created_at);
CREATE INDEX IF NOT EXISTS idx_llm_calls_session ON llm_calls(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_llm_calls_model ON llm_calls(model_id, created_at);
