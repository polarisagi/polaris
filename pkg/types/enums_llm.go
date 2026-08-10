package types

// ============================================================================
// M1 Inference Runtime — LLM 推理控制枚举
// 来源: internal/protocol/types.go §M1
// 架构文档: docs/arch/M01-Inference-Runtime.md
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

// StreamEventType 定义 LLM 流式输出的事件类型。
type StreamEventType int

const (
	StreamTextDelta StreamEventType = iota
	StreamToolCall
	StreamThinking
	StreamError
	// StreamCancelled 用户主动取消时发出，Usage 字段携带补偿计费数据。
	StreamCancelled
	// StreamSystemNotice 系统级提示（非模型输出），Content 是可直接展示给用户的中文文案。
	// 目前唯一生产者是 InferenceRouter 的跨 Model Pool 降级（GD-13-005）：
	// 目标池耗尽并成功降级到备用池时，在流首插入一条告知用户"已切换模型"的提示。
	// 消费方须把它当作旁路信息渲染，**不得**计入助手回复正文或写进消息历史。
	StreamSystemNotice
)

// ModelPool 模型池强类型枚举（ADR-0094 决策八）。
//
// 合法取值唯一 SSoT = internal/llm/router.go 的 poolFallbackChain 降级链
// （reasoning → general → default → budget，见 NewInferenceRouter）。"standard"
// 不是、也从未是合法池名——它是 4 处历史误用（rag_retrieval.go /
// agent/fsm/transitions.go ×2 / eval/analysis/sampling_scorer.go）里凭空出现
// 的字面量，本次订正为下列四个真实枚举值之一，不得再引入。
type ModelPool string

const (
	// ModelPoolReasoning 复杂规划/重规划/高风险决策，成本最高。
	ModelPoolReasoning ModelPool = "reasoning"
	// ModelPoolGeneral 主对话/感知/反思等高频常规任务。
	ModelPoolGeneral ModelPool = "general"
	// ModelPoolDefault 查询分解、质量评分等辅助性任务。
	ModelPoolDefault ModelPool = "default"
	// ModelPoolBudget 级联降级链末档，最低成本。
	ModelPoolBudget ModelPool = "budget"
)
