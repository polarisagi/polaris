package protocol

// TrustTier 五级信任体系（ADR-0016 §2.1）。
// 替代 SignatureValid bool，使系统能区分技能/工具来源的信任级别。
// 级别越高权限越大；只有 Polaris 内部路径可以赋予 TrustSystem。
type TrustTier int

const (
	// TrustUntrusted 无签名或签名校验失败 → fail-closed 拒绝加载。
	TrustUntrusted TrustTier = 0
	// TrustLocal HMAC-SHA256 本地签名（实例密钥，重启后密钥不变则可验证）。
	// 适用于：用户本地开发的 SKILL.md、未上传的 Plugin。
	TrustLocal TrustTier = 1
	// TrustCommunity cosign 签名但 publisher 未在官方白名单。
	// 适用于：开源社区 MCP server、第三方 skill。
	TrustCommunity TrustTier = 2
	// TrustOfficial cosign+OIDC 验证的白名单官方 publisher。
	// 覆盖：OpenAI / Google / Anthropic / MCP 官方 / GitHub / Microsoft / Figma 等。
	// 权限等同于内置技能（approval=auto, Sbx-L2, TaintMedium），无需用户额外审批。
	TrustOfficial TrustTier = 3
	// TrustSystem Polaris 内置，硬编码路径。
	// 只有系统初始化时注册的内置技能和工具可以达到此级别。
	TrustSystem TrustTier = 4
)

// TrustFromSignatureValid 向后兼容转换：SignatureValid bool → TrustTier。
// 用于数据库迁移（021_skill_trust_tier.sql），不在新代码中使用。
func TrustFromSignatureValid(valid bool) TrustTier {
	if valid {
		return TrustCommunity // 保守迁移：签名有效但 publisher 未验证
	}
	return TrustUntrusted
}

// MaxSandboxTier 返回该信任级别允许的最大 Sbx 沙箱级别（1/2/3）。
// M11 PolicyGate 通过此方法约束工具执行的最大沙箱。
func (t TrustTier) MaxSandboxTier() int {
	switch {
	case t >= TrustSystem:
		return 3
	case t >= TrustOfficial:
		return 2
	default:
		return 1
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

// String 返回可读名称（日志 / UI 展示用）。
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
