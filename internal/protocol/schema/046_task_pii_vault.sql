-- task_pii_vault: 任务挂起时的 PII 原文快照（ADR-0104 决策八）
-- AES-256-GCM 密文，禁止进入 preferences/任何列表 API。
-- 写入：Agent 因 provider 熔断挂起时（SessionPIIVault.Snapshot）；
-- 清除：FSM 终态（SecureZero）；expired_at 为毫秒时间戳，过期行读取时忽略。
CREATE TABLE IF NOT EXISTS task_pii_vault (
    task_id    TEXT    NOT NULL,
    field      TEXT    NOT NULL,
    enc_value  TEXT    NOT NULL,
    expired_at INTEGER,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, field)
) STRICT;
