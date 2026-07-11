package types

import "context"

// ============================================================================
// M11 Policy & Safety — 安全层枚举
// 来源: internal/protocol/types.go §TaintLevel, internal/protocol/interfaces.go §M11
// 架构文档: docs/arch/11-Policy-Safety-深度选型.md §2.3
//
// 失败分类枚举
// 来源: internal/protocol/intent.go
//
// 从 enums.go 按模块拆出（R7 文件行数治理，2026-07-07）：原文件中 TrustTier 的
// 类型/常量声明与其方法集（SandboxFloor/TaintLevel/ApprovalRequired/...）在拆分前
// 被黑板事件、失败分类两段无关内容隔开，本次拆分顺带把同一类型的声明与方法
// 物理相邻，不改变任何签名/逻辑。
// ============================================================================

// KillState 紧急停止状态。
type KillState int

const (
	KillNormal   KillState = iota // 正常，没有任何限制
	KillThrottle                  // 降级，停止产生新请求，允许进行中请求完成
	KillPause                     // 暂停，中止并保存进行中请求的状态，可恢复
	KillFullStop                  // 全停，立即中止所有请求并丢弃状态
)

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

// CheckpointDeviceControlReview 电脑/浏览器操控（computer_use/browser_use）的 HITL
// checkpoint 类型，由 interceptComputerUse 发起。
//
// 2026-07-07 从此前复用的 "security_review" 改名拆出：该字符串同时被扩展/插件安装
// 审查（TrustTier + Cedar install_extension_permit 管辖，语义完全不同）复用，导致
// 无法在 resolveTimeoutAction 里按 checkpoint 类型可靠区分"该听用户设备操控权限
// 模式"还是"不该听"。全仓库 grep 确认改名前该字符串无 DB schema / 前端字符串匹配
// 依赖，改名安全。
const CheckpointDeviceControlReview = "device_control_review"

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

type taintContextKey struct{}

// InjectToContext 注入污点到上下文
func (t TaintLevel) InjectToContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, taintContextKey{}, t)
}

// TaintLevelFromContext 从上下文读取污点
func TaintLevelFromContext(ctx context.Context) TaintLevel {
	if v := ctx.Value(taintContextKey{}); v != nil {
		return v.(TaintLevel)
	}
	return TaintNone
}
