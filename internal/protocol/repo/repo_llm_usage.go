package repo

import "context"

// LLMUsageGroupBy 是用量聚合的分组维度（GET /v1/usage 的 group_by）。
type LLMUsageGroupBy string

const (
	LLMUsageByPurpose  LLMUsageGroupBy = "purpose"
	LLMUsageByModel    LLMUsageGroupBy = "model"
	LLMUsageByProvider LLMUsageGroupBy = "provider"
	LLMUsageByDay      LLMUsageGroupBy = "day" // UTC 自然日 YYYY-MM-DD
)

// LLMUsageQuery 用量聚合查询：created_at 落在 [SinceMs, UntilMs) 的 llm_calls 行（Unix 毫秒）。
type LLMUsageQuery struct {
	SinceMs int64
	UntilMs int64
	GroupBy LLMUsageGroupBy
}

// LLMUsageGroup 一个分组的聚合结果。全部为求和，比率由调用方派生（避免把"比率"当可加量存储）。
type LLMUsageGroup struct {
	Key string
	// Requests 该组全部 llm_calls 行数（含 error/cancelled/cache_hit 状态）。
	Requests int64
	// ResponseCacheHits 其中 status=cache_hit 的行数（精确响应缓存命中，未调用 Provider，token 为 0）。
	ResponseCacheHits int64
	// InputTokens 全部输入 token（含前缀缓存命中部分，见 039_llm_calls.sql 与 usage_recorder.go fillUsage 归一）。
	InputTokens     int64
	CacheHitTokens  int64
	OutputTokens    int64 // 含推理
	ReasoningTokens int64
	CostUSD         float64
}

// LLMUsageQueryRepository 只读的 llm_calls 聚合查询（ADR-0105 决策八）。
// 接口由 internal/gateway/server/sysadmin 消费。
//
// @producer: internal/store/repo/repo_llm_usage.go
type LLMUsageQueryRepository interface {
	// AggregateLLMUsage 按 q.GroupBy 聚合。purpose/model/provider 按估算费用降序，day 按日期升序；
	// 组内无行时不返回该组。
	AggregateLLMUsage(ctx context.Context, q LLMUsageQuery) ([]LLMUsageGroup, error)
}

// LLMUsageTotals 一组 llm_calls 行的求和（比率由调用方派生）。
type LLMUsageTotals struct {
	Calls           int64
	InputTokens     int64 // 含前缀缓存命中
	CacheHitTokens  int64
	OutputTokens    int64 // 含推理
	ReasoningTokens int64
	CostUSD         float64
	LatencyMs       int64 // 各次调用耗时之和（并行调用会重复计，仅作参考）
}

// LLMTurnUsage 一个回合（turn_id）的用量。
type LLMTurnUsage struct {
	TurnID    string
	FirstAtMs int64
	LastAtMs  int64
	LLMUsageTotals
}

// LLMSessionUsage 一个会话的全部用途用量（kernel、persona_refine、压缩等，归属以 ctx 的 session_id 为准）。
type LLMSessionUsage struct {
	LLMUsageTotals
	// LastInputTokens/LastModelID：最近一次成功的内核（purpose=kernel）请求的输入 token 与模型，
	// 无内核调用时退回最近一次任意成功请求。供"上下文占用"。
	LastInputTokens int64
	LastModelID     string
	ByTurn          []LLMTurnUsage
}

// LLMSessionUsageRepository 会话/回合/全局维度的用量查询（A3/A4）。
// 与 LLMUsageQueryRepository 由同一个 SQLite 实现满足，消费方按需类型断言。
//
// @producer: internal/store/repo/repo_llm_usage.go
type LLMSessionUsageRepository interface {
	SessionUsage(ctx context.Context, sessionID string) (LLMSessionUsage, error)
	TurnUsage(ctx context.Context, turnID string) (LLMUsageTotals, error)
	// UsageSince 自 sinceMs 起全部会话的累计（含无会话的后台调用）。
	UsageSince(ctx context.Context, sinceMs int64) (LLMUsageTotals, error)
	// ContextWindowFor 探测目录里模型的上下文窗口；未知返回 0。
	ContextWindowFor(ctx context.Context, modelID string) (int, error)
}
