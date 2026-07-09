package types

// ============================================================================
// M1 Inference Runtime — LLM 推理控制枚举
// 来源: internal/protocol/types.go §M1
// 架构文档: docs/arch/01-Inference-Runtime-深度选型.md
//
// 从 enums.go 按模块拆出（R7 文件行数治理，2026-07-07），纯类型/常量声明，
// 无逻辑变更。
// ============================================================================

// ThinkingMode 控制 LLM 的扩展思考深度（TTC: Test-Time Compute）。
// API 参数映射见 docs/arch/M01-Inference-Runtime.md §5.2-bis。
type ThinkingMode string

const (
	// ThinkingDisabled 关闭思考，适用于日常简单请求。
	ThinkingDisabled ThinkingMode = "disabled"

	// ThinkingHigh 高档思考（~100K token 预算），适用于常规规划。
	ThinkingHigh ThinkingMode = "high"

	// ThinkingMax 最大思考（~384K token 预算），适用于失败重规划、高风险任务。
	ThinkingMax ThinkingMode = "max"
)

// ReasoningEffort 跨厂商推理深度枚举（TTC）。
// 各适配器映射: OpenAI→"low"/"medium"/"high", DeepSeek→token_budget, Claude→budget_tokens。
type ReasoningEffort int

const (
	ReasoningEffortNone   ReasoningEffort = iota // 禁用扩展思考，走 System 1
	ReasoningEffortLow                           // 轻量推理（~1K tokens），System 1.5
	ReasoningEffortMedium                        // 标准推理（~8K tokens），System 2 基础
	ReasoningEffortHigh                          // 深度推理（~32K tokens），System 2 完整
)

// StreamEventType 定义 LLM 流式输出的事件类型。
type StreamEventType int

const (
	StreamTextDelta StreamEventType = iota
	StreamToolCall
	StreamThinking
	StreamError
	// StreamCancelled 用户主动取消时发出，Usage 字段携带补偿计费数据。
	StreamCancelled
)
