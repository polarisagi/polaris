package types

// ============================================================================
// 全域物理边界常量
// ============================================================================

const (
	// MaxInlinePayloadBytes 单行热载荷的最大内联大小（4KB）。
	// 超限时必须写入 VFS 并仅存储 vfs://{ref} 指针。
	MaxInlinePayloadBytes = 4096

	// TaintWashingFloor LLM 摘要输出的最低污点等级（硬地板，不可降为 TaintLow）。
	// 用于防止 "污点洗白" 攻击（M11 §2.4）。
	TaintWashingFloor = TaintMedium

	// DefaultStepBudget Agent 默认允许的最大执行步骤数（M4 FSM）。
	DefaultStepBudget = 30

	// DefaultReplanLimit Agent 默认最大重规划次数（M4 ReplanGuard）。
	DefaultReplanLimit = 3

	// DefaultProviderSuspendLimit 连续无可用 Provider 失败次数上限，触发后进入 S_FAILED。
	DefaultProviderSuspendLimit = 5

	// MaxToolTimeout 工具执行的最大超时时间（秒）。
	MaxToolTimeout = 300

	// BlackboardDefaultLeaseTTL 黑板任务认领租约 TTL（秒）。
	BlackboardDefaultLeaseTTL = 60

	// BlackboardHeartbeatInterval 黑板心跳间隔（秒）。
	BlackboardHeartbeatInterval = 15
)
