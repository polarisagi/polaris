// Package policy 提供 polaris 安全策略门与信任档位（ADR-0115 WP1）。
//
// 本文件实现信任档位（TrustProfile）→ 安全策略（Policy）的映射。
// 所有安全闸门应只引用 Policy 字段，不得在闸门逻辑里直接分支 TrustProfile 字符串。
// 违规由 tools/trust_profile_check.go 在 make lint 阶段检测。
package policy

import "github.com/polarisagi/polaris/pkg/types"

// TrustProfile 信任档位标识符（ADR-0115 WP1 决策一）。
// 由 [config.SecurityConfig].TrustProfile 配置，启动时通过 ProfilePolicy 映射为 Policy。
type TrustProfile string

const (
	// ProfileStandard 标准档（默认）：维持现有安全行为。
	ProfileStandard TrustProfile = "standard"
	// ProfileFull 全信任档：宿主直接执行、自动放行 HITL、写前快照、出站只审计。
	ProfileFull TrustProfile = "full"
	// ProfileHardened 强化档：强制容器沙箱、出站记录+阻断、PII 强制开。
	ProfileHardened TrustProfile = "hardened"
)

// Normalize 将空字符串或未知档位归一化到 standard，避免调用点散落校验逻辑。
func (p TrustProfile) Normalize() TrustProfile {
	switch p {
	case ProfileFull, ProfileHardened:
		return p
	default:
		return ProfileStandard
	}
}

// Policy 安全策略快照（ADR-0115 WP1 决策二）。
//
// 所有安全闸门（bash/run_command 沙箱、HITL、出站污点、PII、命令白名单）
// 都通过此 struct 的字段查询行为，而非直接比较 TrustProfile 字符串。
// 这样 ProfilePolicy 是系统里 TrustProfile 语义的唯一权威映射点。
type Policy struct {
	// Profile 原始档位（只读存档，闸门不应直接 switch 此字段，参见 tools/trust_profile_check.go）。
	Profile TrustProfile

	// ForceContainerSandbox hardened 档：强制容器沙箱（覆盖 SandboxConfig.Enabled=false），
	// 即使显式授权也不在宿主执行（ADR-0121 决策一）。
	ForceContainerSandbox bool

	// MaxPermissionMode 权限上限（ADR-0121）：实际模式 = min(偏好 permission_mode, 本值)。
	// standard/full = full_access；hardened = auto_review。
	MaxPermissionMode types.PermissionMode

	// TaintEgressBlockEnabled 出站污点是否阻断（false=只记录不阻断）。
	// standard/hardened 档阻断；full 档只记录。
	TaintEgressBlockEnabled bool

	// PIITokenizeEnabled PII tokenize 功能是否启用。
	// full 档关闭（减少摩擦）；standard/hardened 档开启。
	PIITokenizeEnabled bool

	// CheckpointEnabled 写类 + 高风险操作前是否自动触发影子 git 快照。
	// 仅 full 档开启；standard/hardened 档关闭（快照需要宿主 git 环境访问权限）。
	CheckpointEnabled bool

	// SecretGetEnabled secret_get 内置工具是否注册（凭据可读）。
	// 仅 full 档开启；standard/hardened 档不注册该工具。
	SecretGetEnabled bool

	// SafeDialerAuditOnly SafeDialer/SSRFGuard 是否只记录不阻断。
	// full 档 true（放行公网与内网，只审计）；standard/hardened 档 false（保持阻断）。
	SafeDialerAuditOnly bool

	// RunawayGuardDefaultRounds agentic 循环单回合失控保护默认轮数上限（ADR-0115 WP2）。
	// full 档 0（不限）；standard 档 200；hardened 档 50。
	RunawayGuardDefaultRounds int

	// SubagentMaxConcurrent 子智能体最大并发数（ADR-0115 WP4）。
	// full 档 16；standard 档 6；hardened 档 2；0 = 不限（仅 full 可设）。
	SubagentMaxConcurrent int

	// DelegationChainMaxDepth 委派链最大深度（ADR-0115 WP4）。
	// full 档 8；standard 档 3；hardened 档 2。
	DelegationChainMaxDepth int

	// CodeActTaintBlockEnabled code_act 输出的污点清洗失败时是否阻断（R4-01）。
	// standard/hardened 档阻断（true）；full 档只记录不阻断（false）。
	CodeActTaintBlockEnabled bool

	// JobsKillOnSessionEnd 会话结束时是否清理该会话启动的后台进程（ADR-0115 WP5）。
	// full 档默认 false（保留后台进程）；standard/hardened 档默认 true（回收后台进程）。
	JobsKillOnSessionEnd bool

	// SleepTimeEnabled 是否启用会话空闲 sleep-time 后台整理与自改进闭环（ADR-0115 WP6）。
	// full 档默认 true；standard/hardened 档默认 false。
	SleepTimeEnabled bool

	// SleepTimeIdleMinutes 会话空闲触发阈值（分钟）（ADR-0115 WP6）。
	// 默认 10 分钟。
	SleepTimeIdleMinutes int

	// BackgroundJobsAllowed 是否允许运行后台作业（bash args.Background）。
	// full / standard 档为 true；hardened 档为 false（WP5 ADR-0115 / R3-08）。
	BackgroundJobsAllowed bool

	// PersistentTerminalAllowed 是否允许运行持久终端会话（bash args.SessionID != ""）。
	// full / standard 档为 true；hardened 档为 false（WP5 ADR-0115 / R3-08）。
	PersistentTerminalAllowed bool
}

// IsZero 判断策略是否为空（未初始化）。
func (p Policy) IsZero() bool {
	return p.Profile == ""
}

// ProfilePolicy 将信任档位映射为安全策略快照（ADR-0115 WP1 决策二）。
//
// 这是全系统 TrustProfile 语义的唯一权威实现点。
// 要添加新的安全开关，先在 Policy struct 定义字段，再在此函数三分支里赋值，
// 禁止在其他包里出现 `if profile == "full"` 或同类字符串比较（lint 门控执行）。
func ProfilePolicy(profile TrustProfile) Policy {
	p := profile.Normalize()
	switch p {
	case ProfileFull:
		return Policy{
			Profile:                   ProfileFull,
			ForceContainerSandbox:     false,
			MaxPermissionMode:         types.ModeFullAccess,
			TaintEgressBlockEnabled:   false, // 只记录不阻断
			PIITokenizeEnabled:        false, // 关闭减少摩擦
			CheckpointEnabled:         true,  // 高风险前写前快照
			SecretGetEnabled:          true,  // 凭据可读
			SafeDialerAuditOnly:       true,  // 放行公网+内网，只审计
			RunawayGuardDefaultRounds: 0,     // 不限轮数
			SubagentMaxConcurrent:     16,    // 并发上限更高
			DelegationChainMaxDepth:   8,     // 深度上限更高
			CodeActTaintBlockEnabled:  false, // full 档只记录不阻断
			JobsKillOnSessionEnd:      false, // full 档默认保留后台进程
			SleepTimeEnabled:          true,  // full 档默认开启 sleep-time 后台自改进
			SleepTimeIdleMinutes:      10,    // 默认空闲 10 分钟触发
			BackgroundJobsAllowed:     true,  // full 档允许后台进程
			PersistentTerminalAllowed: true,  // full 档允许持久终端
		}
	case ProfileHardened:
		return Policy{
			Profile:                   ProfileHardened,
			ForceContainerSandbox:     true, // 强制容器沙箱
			MaxPermissionMode:         types.ModeAutoReview,
			TaintEgressBlockEnabled:   true, // 出站记录+阻断
			PIITokenizeEnabled:        true, // 强制 PII 保护
			CheckpointEnabled:         false,
			SecretGetEnabled:          false,
			SafeDialerAuditOnly:       false, // 保持阻断
			RunawayGuardDefaultRounds: 50,    // 严格限制
			SubagentMaxConcurrent:     2,     // 严格并发限制
			DelegationChainMaxDepth:   2,     // 严格深度限制
			CodeActTaintBlockEnabled:  true,  // 强化阻断污点清洗失败
			JobsKillOnSessionEnd:      true,  // 会话结束自动杀后台任务
			SleepTimeEnabled:          false, // hardened 档默认关闭后台自改进
			SleepTimeIdleMinutes:      10,
			BackgroundJobsAllowed:     false, // hardened 档禁止后台进程
			PersistentTerminalAllowed: false, // hardened 档禁止持久终端
		}
	default: // ProfileStandard
		return Policy{
			Profile:                   ProfileStandard,
			ForceContainerSandbox:     false,
			MaxPermissionMode:         types.ModeFullAccess,
			TaintEgressBlockEnabled:   true, // 现状：阻断
			PIITokenizeEnabled:        true, // 现状：开启
			CheckpointEnabled:         false,
			SecretGetEnabled:          false,
			SafeDialerAuditOnly:       false,
			RunawayGuardDefaultRounds: 200,   // 默认防失控
			SubagentMaxConcurrent:     6,     // 默认并发限制
			DelegationChainMaxDepth:   3,     // 默认深度限制（inv_M8_06）
			CodeActTaintBlockEnabled:  true,  // 标准阻断污点清洗失败
			JobsKillOnSessionEnd:      true,  // 会话结束自动杀后台任务
			SleepTimeEnabled:          false, // standard 档默认关闭后台自改进
			SleepTimeIdleMinutes:      10,
			BackgroundJobsAllowed:     true, // standard 档允许沙箱后台进程
			PersistentTerminalAllowed: true, // standard 档允许沙箱持久终端
		}
	}
}

// permissionRank 越大越宽松。
func permissionRank(m types.PermissionMode) int {
	switch m {
	case types.ModeFullAccess:
		return 2
	case types.ModeAutoReview:
		return 1
	}
	return 0 // default 与未知值都取最严
}

// CapMode 把偏好模式压到部署上限之内（取较严者）；零值 Policy（未装配）不设上限。
func (p Policy) CapMode(m types.PermissionMode) types.PermissionMode {
	if p.MaxPermissionMode == "" {
		return m
	}
	if permissionRank(m) > permissionRank(p.MaxPermissionMode) {
		return p.MaxPermissionMode
	}
	return m
}

// StandardPolicy 返回 standard 信任档位的默认策略快照。
func StandardPolicy() Policy {
	return ProfilePolicy(ProfileStandard)
}
