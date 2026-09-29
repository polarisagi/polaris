-- 044_hitl_requests: HITL 审批请求账本（State-in-DB，HE-6；ADR-0104 决策五）
-- 架构角色: automation/hitl.GatewayImpl 是唯一写入点；一行一审批，取代 KV `hitl:pending:*` / `hitl:archive:*`。
--   待审与归档同表同行：条件更新 `UPDATE … WHERE id=? AND status='pending'` 保证重复裁决幂等；
--   进程崩溃遗留的 pending 行（waiter channel 已随进程消失）由启动期孤儿对账
--   （cmd/polaris/boot_orphan_reconciler.go）置为 orphaned，不再是幽灵待审。
-- TrustScorer（GD-14-004）不再持内存计数，直接查本表：只有 decided_by='human' 的行参与信任累积，
--   auto_* / trust_downgrade / timeout_kill / l3_gate 一律不计（避免降级放行反过来加固降级依据的正反馈）。
-- decided_by 语义: human=人工裁决；auto_approve/auto_deny=超时兜底策略；trust_downgrade=信任评分降级放行
--   （审计留痕，非静默）；timeout_kill=kill_pause 超时；l3_gate=L3 回归门禁 P0 失败自动拒绝。
CREATE TABLE IF NOT EXISTS hitl_requests (
    id              TEXT    PRIMARY KEY,                       -- = HITLPrompt.ID
    agent_id        TEXT    NOT NULL DEFAULT '',
    session_id      TEXT    NOT NULL DEFAULT '',               -- 取自 ctx CtxTaskIDKey，无则空
    checkpoint_type TEXT    NOT NULL,
    risk_level      INTEGER NOT NULL DEFAULT 0,
    taint_level     INTEGER NOT NULL DEFAULT 0,
    prompt_json     TEXT    NOT NULL,                          -- 完整 HITLPrompt JSON
    status          TEXT    NOT NULL CHECK(status IN ('pending','approved','denied','timeout','orphaned')),
    decided_by      TEXT    CHECK(decided_by IS NULL OR decided_by IN
                              ('human','auto_approve','auto_deny','trust_downgrade','timeout_kill','l3_gate')),
    reason          TEXT    NOT NULL DEFAULT '',
    response_json   TEXT,                                      -- HITLResponse JSON（仅有应答时）
    deadline_ns     INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,                          -- Unix 毫秒
    decided_at      INTEGER                                    -- Unix 毫秒
) STRICT;

CREATE INDEX IF NOT EXISTS idx_hitl_requests_status ON hitl_requests(status, created_at);
CREATE INDEX IF NOT EXISTS idx_hitl_requests_trust ON hitl_requests(checkpoint_type, agent_id, decided_at);
