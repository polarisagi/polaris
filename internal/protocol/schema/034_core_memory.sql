CREATE TABLE IF NOT EXISTS core_memory_blocks (
    agent_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    block_key TEXT NOT NULL,
    content TEXT NOT NULL,
    taint_level INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (agent_id, session_id, block_key)
);
