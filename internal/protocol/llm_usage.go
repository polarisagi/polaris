package protocol

// LLMUsageRecord 一次 LLM Provider 调用的记账行（llm_calls 表，039_llm_calls.sql）。
// 生产方：internal/llm 注册表的记录包装；消费方：internal/store/repo 落库。
type LLMUsageRecord struct {
	ID              string
	CreatedAtMs     int64
	SessionID       string
	Purpose         string
	Provider        string
	ModelID         string
	ModelPool       string
	ThinkingMode    string
	Streaming       bool
	Status          string // LLMUsageStatusOK / Error / Cancelled
	InputTokens     int    // 含缓存命中
	CacheHitTokens  int
	OutputTokens    int // 含推理
	ReasoningTokens int
	LatencyMs       int64
	CostUSD         float64
	Error           string
}

// LLMUsageRecord.Status 取值。
const (
	LLMUsageStatusOK        = "ok"
	LLMUsageStatusError     = "error"
	LLMUsageStatusCancelled = "cancelled"
)
