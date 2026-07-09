package types

// ============================================================================
// M7 Tool & Action — 工具层枚举
// 来源: internal/protocol/types.go §M7
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §3
//
// 从 enums.go 按模块拆出（R7 文件行数治理，2026-07-07），纯类型/常量声明，
// 无逻辑变更。
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
