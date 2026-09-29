-- 047_llm_response_cache.sql
-- 确定性后台 LLM 调用的精确匹配响应缓存（ADR-0105 决策六）。
-- 仅缓存「相同输入必得相同输出」的后台用途（graphrag_* / rag_summary_tree / rag_query_rewrite /
-- memory_write_filter），内核阶段（perceive/plan/reflect/respond/validate）硬性排除。
-- 写入/读取方：internal/llm 注册表的记录包装，经 internal/store/repo.SQLiteLLMResponseCacheRepository。
-- 条数有界（llm.response_cache.max_entries，写入后淘汰最旧），过期行由周期清理删除。
CREATE TABLE IF NOT EXISTS llm_response_cache (
    key         TEXT PRIMARY KEY,             -- sha256(provider|model|purpose|max_tokens|top_p|response_format|messages)
    purpose     TEXT    NOT NULL,             -- InferOptions.Purpose（白名单内的后台用途）
    provider    TEXT    NOT NULL,             -- 注册名（与 llm_calls.provider 同口径）
    model       TEXT    NOT NULL DEFAULT '',  -- 实际模型 ID（请求显式指定优先，否则 Provider.ModelID()）
    response    TEXT    NOT NULL,             -- 序列化的响应（JSON：content/reasoning_content/model/finish_reason）
    created_at  INTEGER NOT NULL,             -- Unix 毫秒；有界淘汰按此升序删除最旧
    expires_at  INTEGER NOT NULL,             -- Unix 毫秒；过期行读取时视为未命中
    hit_count   INTEGER NOT NULL DEFAULT 0    -- 命中次数（仅供观测，异步累加，允许少计）
);

CREATE INDEX IF NOT EXISTS idx_llm_response_cache_expires_at ON llm_response_cache(expires_at);
CREATE INDEX IF NOT EXISTS idx_llm_response_cache_created_at ON llm_response_cache(created_at);
