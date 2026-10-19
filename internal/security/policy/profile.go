// Package policy 提供 polaris 安全策略门与信任档位（ADR-0115 WP1）。
//
// 本文件实现信任档位（TrustProfile）→ 安全策略（Policy）的映射。
// 所有安全闸门应只引用 Policy 字段，不得在闸门逻辑里直接分支 TrustProfile 字符串。
// 违规由 tools/trust_profile_check.go 在 make lint 阶段检测。
package policy

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

	// SandboxDisabled full 档：bash/run_command 在宿主直接执行，忽略 sandboxEnabled 配置。
	// standard/hardened 档遵从 SandboxConfig.Enabled 原有行为。
	SandboxDisabled bool

	// ForceContainerSandbox hardened 档：强制容器沙箱（覆盖 SandboxConfig.Enabled=false）。
	// SandboxDisabled 与 ForceContainerSandbox 互斥，ProfilePolicy 保证二者不同时为 true。
	ForceContainerSandbox bool

	// AllowedPathsOpen full 档：bash 执行时不受路径白名单限制（宿主工作目录）。
	// 注意：read_file/write_file 等文件类工具的 AllowedPaths 仍由 SandboxConfig 控制，
	// 本字段仅影响 bash/run_command 的 workDir 与 AllowedPaths 传参行为。
	AllowedPathsOpen bool

	// HITLAutoApprove full 档：高风险命令自动审批通过（不阻塞等待人工）。
	// 自动放行同时写审计日志（HE-1 要求每条高风险操作必须留追踪记录）。
	HITLAutoApprove bool

	// TaintEgressBlockEnabled 出站污点是否阻断（false=只记录不阻断）。
	// standard/hardened 档阻断；full 档只记录。
	TaintEgressBlockEnabled bool

	// PIITokenizeEnabled PII tokenize 功能是否启用。
	// full 档关闭（减少摩擦）；standard/hardened 档开启。
	PIITokenizeEnabled bool

	// RunCmdWhitelistEnabled run_command 命令前缀白名单是否生效。
	// full 档关闭（宿主直接执行，白名单无意义）；standard/hardened 档开启。
	RunCmdWhitelistEnabled bool

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

	// CodeActAllToolsAllowed code_act 工具桥是否允许调用所有工具（ADR-0115 WP5）。
	// full 档 true（放行全部工具）；standard/hardened 档 false（仅允许 CapReadOnly 且幂等的只读工具）。
	CodeActAllToolsAllowed bool

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
			SandboxDisabled:           true,  // 宿主直接执行（无路径白名单）
			ForceContainerSandbox:     false, // 互斥
			AllowedPathsOpen:          true,  // bash 不受路径白名单限制
			HITLAutoApprove:           true,  // 高风险前自动放行+审计+快照
			TaintEgressBlockEnabled:   false, // 只记录不阻断
			PIITokenizeEnabled:        false, // 关闭减少摩擦
			RunCmdWhitelistEnabled:    false, // 宿主执行，白名单无意义
			CheckpointEnabled:         true,  // 高风险前写前快照
			SecretGetEnabled:          true,  // 凭据可读
			SafeDialerAuditOnly:       true,  // 放行公网+内网，只审计
			RunawayGuardDefaultRounds: 0,     // 不限轮数
			SubagentMaxConcurrent:     16,    // 并发上限更高
			DelegationChainMaxDepth:   8,     // 深度上限更高
			CodeActAllToolsAllowed:    true,  // full 档允许在 code_act 里调用全部工具
			JobsKillOnSessionEnd:      false, // full 档默认保留后台进程
			SleepTimeEnabled:          true,  // full 档默认开启 sleep-time 后台自改进
			SleepTimeIdleMinutes:      10,    // 默认空闲 10 分钟触发
			BackgroundJobsAllowed:     true,  // full 档允许后台进程
			PersistentTerminalAllowed: true,  // full 档允许持久终端
		}
	case ProfileHardened:
		return Policy{
			Profile:                   ProfileHardened,
			SandboxDisabled:           false,
			ForceContainerSandbox:     true, // 强制容器沙箱
			AllowedPathsOpen:          false,
			HITLAutoApprove:           false, // 严格 HITL 审批
			TaintEgressBlockEnabled:   true,  // 出站记录+阻断
			PIITokenizeEnabled:        true,  // 强制 PII 保护
			RunCmdWhitelistEnabled:    true,  // 白名单严格
			CheckpointEnabled:         false,
			SecretGetEnabled:          false,
			SafeDialerAuditOnly:       false, // 保持阻断
			RunawayGuardDefaultRounds: 50,    // 严格限制
			SubagentMaxConcurrent:     2,     // 严格并发限制
			DelegationChainMaxDepth:   2,     // 严格深度限制
			CodeActAllToolsAllowed:    false, // 只允许 CapReadOnly 幂等工具
			JobsKillOnSessionEnd:      true,  // 会话结束自动杀后台任务
			SleepTimeEnabled:          false, // hardened 档默认关闭后台自改进
			SleepTimeIdleMinutes:      10,
			BackgroundJobsAllowed:     false, // hardened 档禁止后台进程
			PersistentTerminalAllowed: false, // hardened 档禁止持久终端
		}
	default: // ProfileStandard
		return Policy{
			Profile:                   ProfileStandard,
			SandboxDisabled:           false,
			ForceContainerSandbox:     false,
			AllowedPathsOpen:          false,
			HITLAutoApprove:           false,
			TaintEgressBlockEnabled:   true, // 现状：阻断
			PIITokenizeEnabled:        true, // 现状：开启
			RunCmdWhitelistEnabled:    true, // 现状：白名单
			CheckpointEnabled:         false,
			SecretGetEnabled:          false,
			SafeDialerAuditOnly:       false,
			RunawayGuardDefaultRounds: 200,   // 默认防失控
			SubagentMaxConcurrent:     6,     // 默认并发限制
			DelegationChainMaxDepth:   3,     // 默认深度限制（inv_M8_06）
			CodeActAllToolsAllowed:    false, // 只允许 CapReadOnly 幂等工具
			JobsKillOnSessionEnd:      true,  // 会话结束自动杀后台任务
			SleepTimeEnabled:          false, // standard 档默认关闭后台自改进
			SleepTimeIdleMinutes:      10,
			BackgroundJobsAllowed:     true, // standard 档允许沙箱后台进程
			PersistentTerminalAllowed: true, // standard 档允许沙箱持久终端
		}
	}
}

// StandardPolicy 返回 standard 信任档位的默认策略快照。
func StandardPolicy() Policy {
	return ProfilePolicy(ProfileStandard)
}
