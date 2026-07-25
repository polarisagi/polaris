package sandbox

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// PersistentSandbox 是 D4（原 GD-14-003，ADR-0078）Sandbox-L4-Persistent 的
// 骨架实现——路由/硬件门控/配置阈值全部接线完毕，但底层 checkpoint/restore
// 能力**诚实留空**，不冒充已具备真实持久化能力。
//
// 背景：原始设计设想用 CRIU（Linux）或 Firecracker microVM snapshot 实现
// 长程有状态 CodeAct 会话的进程级快照/恢复。但本仓库现有 L3 沙箱
// （sandbox_container.go）已在 ADR-0008/ADR-0011 明确废弃容器运行时/虚拟化
// 路径，统一收敛为 bwrap（Linux namespace）/Seatbelt（macOS sandbox profile）
// 进程级隔离——两者都没有对应的 checkpoint/restore 原语；CRIU 理论上能对
// bwrap 派生的普通 Linux 进程树做 dump，但需要额外的 PID namespace 边界工程
// 且仅覆盖 Linux，macOS 完全没有等价机制；Firecracker/gVisor/microVM 在本仓库
// 是已废弃方案，没有可复用代码。在没有真实宿主验证 CRIU 端到端可用性的前提
// 下实现一个"看起来能用但实际不 dump/restore 任何东西"的假后端，属于
// HE-2（可验证执行）明令禁止的"概率过滤/伪装当安全边界"同类问题的另一种
// 表现——伪装能力比不实现更危险。
//
// 因此本次交付范围（用户已确认选择"接线到位，后端诚实留空"）：
//   - SandboxProvider 接口满足（Run 方法）；
//   - Available() 恒定返回 false，附带清晰的"为什么"注释；
//   - Run() 作为纵深防御第二道闸门，即使调用方绕过 Available() 检查误调用，
//     也明确返回 apperr.CodeUnimplemented 而非静默假装成功或崩溃；
//   - sandbox_router.go 已打好 case types.SandboxPersistent 分支，Available()
//     为 false 时按既有 fallback 链降级（与 SandboxContainer 分支一致的降级
//     语义），不影响 Tier-0/Tier-1 任何现有行为；
//   - 硬件门控 + 配置阈值（sandbox.l4_enabled/sandbox.l4_backend）已落地，
//     默认关闭。
//
// 未来真正接入 CRIU/等价机制时，只需替换本文件的 Available()/Run() 实现，
// 路由层/配置层/硬件门控均无需改动（R1.4 消费方接口 + 组合原语的价值：
// 变更面被限制在这一个文件内）。
type PersistentSandbox struct {
	// backend 记录期望使用的后端标识（来自配置 sandbox.l4_backend），仅用于
	// 日志/诊断；不影响 Available() 的判定——即便运营者显式配置了后端名称，
	// 在没有真实实现之前 Available() 依然恒定返回 false。
	backend string
}

// NewPersistentSandbox 构造 D4 持久化沙箱骨架。backend 为运营者在配置中声明
// 期望使用的后端标识（如 "criu"、"docker_checkpoint"），当前仅用于日志。
func NewPersistentSandbox(backend string) *PersistentSandbox {
	if backend == "" {
		backend = "unimplemented"
	}
	return &PersistentSandbox{backend: backend}
}

// Available 报告 L4 持久化沙箱当前是否可用。
//
// 恒定返回 false：本仓库尚未选型/集成任何真实的 checkpoint/restore 后端（见
// 类型注释）。调用方（sandbox_router.go RouteByTier）必须在 Available()==false
// 时降级到既有路径，不得假设"配置里声明了 backend 名字"就等同于可用。
//
// 这是本文件中当"未来真正接入 CRIU/等价机制"时唯一需要替换判定逻辑的
// 位置——届时应替换为真实的宿主能力探测（如检测 criu 二进制 + 内核
// checkpoint/restore 支持，或 dockerd experimental checkpoint 特性开关）。
func (p *PersistentSandbox) Available() bool {
	return false
}

// Backend 返回期望的后端标识（诊断用途）。
func (p *PersistentSandbox) Backend() string {
	return p.backend
}

// Run 实现 SandboxProvider。纵深防御：Available()==false 时理论上不应被
// 路由层调用到，但仍显式返回 CodeUnimplemented 而非静默假装执行成功——
// 避免"接口占位"被误用为"已实现"。
func (p *PersistentSandbox) Run(_ context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	return nil, apperr.New(apperr.CodeUnimplemented,
		"sandbox: L4 persistent checkpoint/restore backend not implemented (backend="+p.backend+", tool="+spec.ToolName+")")
}
