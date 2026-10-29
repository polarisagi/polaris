-- 032_mock_response_cache: WASI Dry-Run MockProxy 响应缓存
-- 架构角色: PlannerPool 预生成的 Mock 响应表，避免 MCTS 试运行产生真实副作用
CREATE TABLE IF NOT EXISTS mock_response_cache (
    operation_hash  TEXT    PRIMARY KEY,   -- SHA256(method+url+body前1KB)，前32字节hex
    plan_session_id TEXT    NOT NULL,      -- 关联 planner_sessions.id
    method          TEXT    NOT NULL,      -- HTTP 方法
    url_pattern     TEXT    NOT NULL,      -- URL（可含通配符前缀）
    status_code     INTEGER NOT NULL DEFAULT 200,
    response_body   TEXT    NOT NULL DEFAULT '{}',
    hit_count       INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER               -- 为 NULL 则跟随 planner_session 生命周期
) STRICT;
