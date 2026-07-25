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
	// SandboxContainer L3：实际由 Rust FFI 统一实现（Linux=bwrap namespace 隔离，
	// macOS=Seatbelt sandbox profile），通过 CmdRunner 抽象跨平台调用，见
	// internal/sandbox/sandbox_container.go。注释历史上写过 "gVisor / microVM"，
	// 但 ADR-0008/ADR-0011 已将其收敛为 bwrap/Seatbelt，不涉及容器运行时或
	// 虚拟化——2026-07-25 D4 复核订正此处过时描述（comment-drift）。
	SandboxContainer
	// SandboxRemote 委托给远端 HTTP 执行器，用于 Tier-0 内存受限时外包重计算任务。
	SandboxRemote
	// SandboxNativeOS Rust 原生 OS 沙箱（bwrap/Seatbelt）。
	// Tier-0（2GB VPS）上 SandboxContainer 的 fallback：无需容器运行时，
	// 直接通过 Rust FFI 调用宿主 OS 隔离原语（Linux=bwrap, macOS=Seatbelt）。
	// assign.go：SandboxContainer + hwTier==0 → 自动降级为此 tier。
	SandboxNativeOS
	// SandboxPersistent D4（原 GD-14-003，ADR-0078）：Tier2+ 可选持久化沙箱
	// （Sandbox-L4-Persistent，命名沿用设计文档；数值上是本枚举第 6 个 tier，
	// 与既有注释中散称的 "L4"（SandboxRemote/SandboxNativeOS 的口语化标签，
	// 非严格序号）不是同一含义，避免混淆特此说明）。
	//
	// 现状（诚实占位，非完整实现）：内置沙箱架构（bwrap/Seatbelt，见上）没有
	// 对应的 checkpoint/restore 原语，本仓库也未选型/集成 CRIU 或等价机制
	// （历史上考虑过的 Firecracker/gVisor/microVM 路径已在 ADR-0008/ADR-0011
	// 废弃，没有可复用实现）。本 tier 的路由分支、硬件门控、配置阈值均已接线
	// 完整，但底层 internal/sandbox.PersistentSandbox.Available() 恒定返回
	// false——即这是一条已铺好但当前不可达的路径，不冒充已具备真实
	// checkpoint/restore 能力。详见 sandbox_persistent.go 与 ADR-0078。
	SandboxPersistent
)

// ToolSource 标识工具的来源类型（影响 TrustTier 和 TaintLevel 传播）。
type ToolSource string

const (
	ToolBuiltin      ToolSource = "builtin"
	ToolMCP          ToolSource = "mcp"
	ToolSkill        ToolSource = "skill"
	ToolA2A          ToolSource = "a2a"
	ToolLLMGenerated ToolSource = "llm_generated"
	SkillPrefix                 = string(ToolSkill) + ":"
)
