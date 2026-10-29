package repo

import "context"

// LLMCacheEntry 是 llm_response_cache 表的一行（047_llm_response_cache.sql）。
type LLMCacheEntry struct {
	Key         string
	Purpose     string
	Provider    string
	Model       string
	Response    string // 序列化响应（JSON），由 internal/llm 编解码
	CreatedAtMs int64
	ExpiresAtMs int64
	HitCount    int64
}

// LLMResponseCacheRepository 确定性后台 LLM 调用的精确匹配响应缓存持久化（ADR-0105 决策六）。
// 接口由调用方 internal/llm 消费。
//
// @producer: internal/store/repo/repo_llm_response_cache.go
type LLMResponseCacheRepository interface {
	// Get 返回未过期的条目；未命中或已过期返回 (nil, nil)。
	Get(ctx context.Context, key string, nowMs int64) (*LLMCacheEntry, error)
	// Put 写入或覆盖一条（同 key 覆盖，命中计数归零）。
	Put(ctx context.Context, e LLMCacheEntry) error
	// IncrHit 命中计数 +1；key 不存在时静默忽略。仅供观测，调用方可异步/丢弃。
	IncrHit(ctx context.Context, key string) error
	// EvictOverflow 删除最旧的条目，使总行数不超过 maxEntries；返回删除行数。
	// maxEntries <= 0 视为无效，不做任何事（有界性由调用方保证传入正值）。
	EvictOverflow(ctx context.Context, maxEntries int) (int64, error)
	// DeleteExpired 删除 expires_at <= nowMs 的行；返回删除行数。
	DeleteExpired(ctx context.Context, nowMs int64) (int64, error)
}
