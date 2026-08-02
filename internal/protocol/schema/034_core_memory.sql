CREATE TABLE IF NOT EXISTS core_memory_blocks (
    agent_id   TEXT NOT NULL,
    session_id TEXT NOT NULL,
    block_key  TEXT NOT NULL,
    content    TEXT NOT NULL,
    taint_level INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    description TEXT NOT NULL DEFAULT '',
    -- ↑ 块用途的一句话说明（Agent 自述）。随 list 返回，帮助模型在多块场景下
    --   正确选择编辑目标，避免"写错块"这一 MemFS 实践中的主要失败模式（ADR-0082）。

    read_only  INTEGER NOT NULL DEFAULT 0,
    -- ↑ 1 = 保护块，core_memory_edit 的 set/append/replace/delete 一律拒绝（CodeForbidden）。
    --   用于 persona / 安全约束等由系统写入、不允许模型自改的块，防自我越权（ADR-0082）。

    max_bytes  INTEGER NOT NULL DEFAULT 2048,
    -- ↑ 单块内容字节上限，新建行时由 config.M5MemoryThresholds.CoreMemoryBlockMaxKB
    --   （state.yaml core_memory_block_max_kb SSoT，当前 2KB）固化写入，不追溯已存在行。
    --   超限写入被拒绝并在工具返回中提示当前大小与上限，而非静默截断（ADR-0082）。

    PRIMARY KEY (agent_id, session_id, block_key)
);
