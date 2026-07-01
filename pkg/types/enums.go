package types

// ============================================================================
// M4 Agent Kernel — FSM 状态枚举
// 来源: internal/protocol/types.go §M4
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §1
// 11 状态: 5 主执行态 + 2 恢复态 + 2 终态 + 1 中断暂停态 + 1 挂起态
// ============================================================================

// AgentState 定义 Agent FSM 的全部状态枚举（全系统唯一权威定义）。
type AgentState int

const (
	AgentStateIdle      AgentState = iota // S_IDLE: 空闲等待意图
	AgentStatePerceive                    // S_PERCEIVE: LLM 填槽理解任务
	AgentStatePlan                        // S_PLAN: LLM 填槽生成 DAG
	AgentStateValidate                    // S_VALIDATE: 四层校验
	AgentStateExecute                     // S_EXECUTE: DAG 执行
	AgentStateReflect                     // S_REFLECT: LLM 填槽反思
	AgentStateReplan                      // S_REPLAN: 重新规划（Recovery）
	AgentStateRollback                    // S_ROLLBACK: Saga 逆序补偿（Recovery）
	AgentStateComplete                    // S_COMPLETE: 成功终态
	AgentStateFailed                      // S_FAILED: 失败终态（ReplanGuard 超限）
	AgentStateInterrupt                   // S_INTERRUPT: 用户中断暂停态（非终态，可 Resume/Redirect/Abort）
	AgentStateSuspended                   // S_SUSPENDED: 空闲挂起（Suspend-on-Idle）
)

// AgentTrigger 定义驱动 FSM 状态转移的触发器枚举。
type AgentTrigger int

const (
	TriggerIntentReceived AgentTrigger = iota
	TriggerPerceiveDone
	TriggerPlanDone
	TriggerValidateOk
	TriggerValidateFail
	TriggerExecuteDone
	TriggerExecuteFail
	TriggerReflectDone
	TriggerRollbackDone
	TriggerReplanDone
	TriggerReplanExhausted
	TriggerInterruptReceived // 用户中断信号（任意活跃态均可接收，inv_global_08）
	TriggerInterruptResume   // 中断后恢复执行（回到 interruptFrom 状态）
	TriggerInterruptAbort    // 中断后终止任务 → S_FAILED
	TriggerSuspend           // Suspend-on-Idle 触发挂起
	TriggerResume            // 从挂起状态恢复 → S_IDLE
)

// InterruptAction 定义中断处理语义。
// 来源: internal/protocol/interfaces.go §Agent 控制接口
type InterruptAction int

const (
	InterruptResume   InterruptAction = iota // 恢复执行（回到被中断的状态）
	InterruptRedirect                        // 重新规划（新意图 → S_PERCEIVE）
	InterruptAbort                           // 终止任务 → S_FAILED
)

// ============================================================================
// M1 Inference Runtime — LLM 推理控制枚举
// 来源: internal/protocol/types.go §M1
// 架构文档: docs/arch/01-Inference-Runtime-深度选型.md
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

// ============================================================================
// M2 Storage Fabric — 存储操作枚举
// 来源: internal/protocol/types.go §M2
// ============================================================================

// OpType 定义批量写操作的类型。
type OpType int

const (
	OpPut OpType = iota
	OpDelete
)

// ============================================================================
// M5/M10 — 检索层枚举
// 来源: internal/protocol/types.go §M5/M10
// ============================================================================

// EvidenceType 标注检索结果的证据来源（HE-Rule-1 Surprise_Index）。
// Agent 可据此决策是否需要二次验证。
type EvidenceType string

const (
	EvidenceExactMatch   EvidenceType = "exact_match"   // 精确标题/关键字命中
	EvidenceHighVector   EvidenceType = "high_vector"   // 向量相似度 > 0.85
	EvidenceFTSKeyword   EvidenceType = "fts_keyword"   // BM25 全文检索命中
	EvidenceWeakSemantic EvidenceType = "weak_semantic" // 弱语义相似（向量 <= 0.85）
)

// ============================================================================
// M7 Tool & Action — 工具层枚举
// 来源: internal/protocol/types.go §M7
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §3
// ============================================================================

// CapabilityLevel 定义工具的能力级别（由低到高）。
type CapabilityLevel int

const (
	CapReadOnly     CapabilityLevel = iota // 只读操作
	CapWriteLocal                          // 本地写操作
	CapWriteNetwork                        // 网络写操作
	CapPrivileged                          // 特权操作
)

// SideEffect 描述工具执行的副作用类型（用于 Saga 补偿策略选择）。
type SideEffect string

const (
	SideFileWrite    SideEffect = "file_write"
	SideNetworkCall  SideEffect = "network_call"
	SideProcessSpawn SideEffect = "process_spawn"
	SideStateMutate  SideEffect = "state_mutate"
	SideNone         SideEffect = "none"
)

// RiskLevel 工具风险等级（影响沙箱选择和 HITL 触发阈值）。
type RiskLevel int

const (
	RiskLow RiskLevel = iota
	RiskMedium
	RiskHigh
	RiskPrivileged
)

// SandboxTier 沙箱隔离级别（Sbx-L1/L2/L3/Remote）。
// 数值从 1 开始对应文档中的 L1~L3 编号。
type SandboxTier int

const (
	SandboxInProcess SandboxTier = iota + 1 // L1: 进程内隔离
	SandboxWasm                             // L2: Wasmtime 沙箱
	SandboxContainer                        // L3: gVisor / microVM
	// SandboxRemote 委托给远端 HTTP 执行器，用于 Tier-0 内存受限时外包重计算任务。
	SandboxRemote
	// SandboxNativeOS Rust 原生 OS 沙箱（bwrap/Seatbelt）。
	// Tier-0（2GB VPS）上 SandboxContainer 的 fallback：无需容器运行时，
	// 直接通过 Rust FFI 调用宿主 OS 隔离原语（Linux=bwrap, macOS=Seatbelt）。
	// assign.go：SandboxContainer + hwTier==0 → 自动降级为此 tier。
	SandboxNativeOS
)

// ToolSource 标识工具的来源类型（影响 TrustTier 和 TaintLevel 传播）。
type ToolSource string

const (
	ToolBuiltin      ToolSource = "builtin"
	ToolMCP          ToolSource = "mcp"
	ToolSkill        ToolSource = "skill"
	ToolA2A          ToolSource = "a2a"
	ToolLLMGenerated ToolSource = "llm_generated"
)

// ============================================================================
// M8 Multi-Agent Orchestrator — 任务状态枚举
// 来源: internal/protocol/types.go §M8
// 架构文档: docs/arch/08-Multi-Agent-Orchestrator-深度选型.md §1
// ============================================================================

// TaskStatus 定义黑板任务的生命周期状态。
type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskClaimed
	TaskExecuting
	TaskSuspended
	TaskCompensating
	TaskDone
	TaskFailed
)

// VerificationVerdict 流水线验证阶段的结论等级。
type VerificationVerdict int

const (
	VerdictPass    VerificationVerdict = iota // 目标已达成
	VerdictWarning                            // 有不确定项，人工决策
	VerdictBlocker                            // 目标未达成，必须重规划
)

// String 返回可读字符串，实现 fmt.Stringer（标准库要求，L0 例外）。
func (v VerificationVerdict) String() string {
	switch v {
	case VerdictPass:
		return "PASS"
	case VerdictWarning:
		return "WARNING"
	case VerdictBlocker:
		return "BLOCKER"
	default:
		return "UNKNOWN"
	}
}

// ============================================================================
// M11 Policy & Safety — 安全层枚举
// 来源: internal/protocol/types.go §TaintLevel, internal/protocol/interfaces.go §M11
// 架构文档: docs/arch/11-Policy-Safety-深度选型.md §2.3
// ============================================================================

// TaintLevel 全系统污点置信度枚举（全局字典: docs/arch/00-Global-Dictionary.md §4）。
// 传播规则: output = max(所有输入的 TaintLevel)，只升不降。
type TaintLevel int

const (
	TaintNone         TaintLevel = iota // 系统生成/常量，无污染
	TaintLow                            // 受信内部数据
	TaintMedium                         // LLM 摘要输出（硬地板，不可降为 Low）
	TaintHigh                           // 外部用户输入
	TaintUserReviewed                   // 人类显式确认
)

// String 返回可读字符串（标准库 fmt.Stringer，L0 例外）。
func (t TaintLevel) String() string {
	switch t {
	case TaintNone:
		return "none"
	case TaintLow:
		return "low"
	case TaintMedium:
		return "medium"
	case TaintHigh:
		return "high"
	case TaintUserReviewed:
		return "user_reviewed"
	default:
		return "unknown"
	}
}

// PermissionMode 定义外部扩展调用的权限模式。
// 来源: internal/protocol/interfaces.go §M11
type PermissionMode string

const (
	ModeDefault    PermissionMode = "default"
	ModeAutoReview PermissionMode = "auto_review"
	ModeFullAccess PermissionMode = "full_access"
)

// TrustTier 五级信任体系（ADR-0016 §2.1）。
// 来源: internal/protocol/trust.go
// 替代 SignatureValid bool，使系统能区分技能/工具来源的信任级别。
// 业务方法（MaxSandboxTier、ApprovalRequired 等）保留在 internal/protocol/trust.go。
type TrustTier int

const (
	// TrustUntrusted 无签名或签名校验失败 → fail-closed 拒绝加载。
	TrustUntrusted TrustTier = 0
	// TrustLocal HMAC-SHA256 本地签名（实例密钥）。
	TrustLocal TrustTier = 1
	// TrustCommunity cosign 签名但 publisher 未在官方白名单。
	TrustCommunity TrustTier = 2
	// TrustOfficial cosign+OIDC 验证的白名单官方 publisher。
	TrustOfficial TrustTier = 3
	// TrustSystem Polaris 内置，硬编码路径，只有系统初始化时注册的内置技能可达。
	TrustSystem TrustTier = 4
)

// String 返回可读名称（日志 / UI 展示用，标准库 fmt.Stringer，L0 例外）。
func (t TrustTier) String() string {
	switch t {
	case TrustSystem:
		return "system"
	case TrustOfficial:
		return "official"
	case TrustCommunity:
		return "community"
	case TrustLocal:
		return "local"
	default:
		return "untrusted"
	}
}

// ============================================================================
// 黑板事件 & 出箱状态枚举
// 来源: internal/protocol/event.go
// ============================================================================

// EventType 枚举黑板协调事件类型（跨模块走结构化事件，XR-04）。
type EventType string

const (
	EventIntent        EventType = "intent"
	EventClaim         EventType = "claim"
	EventResult        EventType = "result"
	EventFail          EventType = "fail"
	EventCancel        EventType = "cancel"
	EventHeartbeat     EventType = "heartbeat"
	EventActionPending EventType = "action_pending"
	EventActionDone    EventType = "action_done"

	// M9 自改善闭环事件
	EventHeuristicGenerated EventType = "heuristic_generated"
	EventEvalCompleted      EventType = "eval_completed"
)

// EventStatus 跟踪事件生命周期。
type EventStatus string

const (
	StatusPending   EventStatus = "pending"
	StatusClaimed   EventStatus = "claimed"
	StatusExecuting EventStatus = "executing"
	StatusDone      EventStatus = "done"
	StatusFailed    EventStatus = "failed"
)

// OutboxStatus 出箱记录状态。
type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "pending"
	OutboxProcessing OutboxStatus = "processing"
	OutboxDone       OutboxStatus = "done"
	OutboxDead       OutboxStatus = "dead" // 超过 MaxRetries
)

// ============================================================================
// 失败分类枚举
// 来源: internal/protocol/intent.go
// ============================================================================

// FailureClass 区分不可控基础设施失败与逻辑错误。
// 用于自改善引擎和技能生命周期：避免将瞬时基础设施故障计入质量指标。
type FailureClass string

const (
	FailureLogic          FailureClass = "logic"          // 推理错误、计划失败、技能错误
	FailureControllable   FailureClass = "controllable"   // 超时、资源耗尽（系统仍健康）
	FailureUncontrollable FailureClass = "uncontrollable" // 网络离线、提供商宕机、配额耗尽
)

// SandboxFloor 返回该信任级别要求的【最低】沙箱隔离等级（floor，下限）。
// 信任越低，强制隔离越高。调用方不得降级，取 max(SandboxFloor, 其他底线)。
// 唯一权威：TrustTier → SandboxTier。Container(L3) 不由信任触发，仅由 Capability/SideEffect 触发。
func (t TrustTier) SandboxFloor() SandboxTier {
	switch {
	case t >= TrustOfficial: // 3, 4：制品签名/内置，等同完全信任
		return SandboxInProcess // L1
	default: // Community(2) / Local(1) / Untrusted(0)
		return SandboxWasm // L2：无系统调用强隔离，Tier-0 可运行
	}
}

// TaintLevel 返回工具/MCP 输出的 Taint 标记级别。
// 0=None（不污染），1=Medium，2=High。
// 与 M11 TaintLevel 枚举对应（数值相同）。
func (t TrustTier) TaintLevel() int {
	switch {
	case t >= TrustSystem:
		return 0 // TaintNone：内置工具输出不污染上下文
	case t >= TrustOfficial:
		return 1 // TaintMedium：官方来源，可信但非内置
	default:
		return 2 // TaintHigh：社区/本地/未知来源
	}
}

// ApprovalRequired 返回该信任级别的工具调用是否需要用户审批确认。
// TrustOfficial 及以上不需要（与内置工具等同），以下需要。
func (t TrustTier) ApprovalRequired() bool {
	return t < TrustOfficial
}

// MCPApprovalMode 返回 MCP server 的默认 approval 模式字符串。
// 对应 Codex default_tools_approval_mode：auto / prompt / approve。
func (t TrustTier) MCPApprovalMode() string {
	if t >= TrustOfficial {
		return "auto"
	}
	return "prompt"
}

// Trusted 返回对应 MCPClientConfig.Trusted 布尔值（向后兼容桥接）。
// TrustOfficial 及以上视为 trusted → TaintMedium（M7 inv_M7_02）。
func (t TrustTier) Trusted() bool {
	return t >= TrustOfficial
}

// PropagateTaint 计算输出污点等级 = max(所有输入).
func PropagateTaint(inputs ...TaintLevel) TaintLevel {
	var max TaintLevel
	for _, t := range inputs {
		if t > max {
			max = t
		}
	}
	return max
}
