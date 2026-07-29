-- ============================================================================
-- Schema: World Model Edges (M05 Memory System - Synaptic Plasticity)
-- ============================================================================

CREATE TABLE IF NOT EXISTS world_model_edges (
    edge_id TEXT PRIMARY KEY,
    -- ↑ 关系边唯一标识。

    storage_strength REAL DEFAULT 1.0,
    -- ↑ 长期记忆强度，随复用次数增长，慢衰减。

    retrieval_strength REAL DEFAULT 1.0,
    -- ↑ 短期可提取性，随时间快衰减，成功提取后跳升。

    last_accessed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
    -- ↑ 最后访问时间，用于读时衰减计算。
);
