package types

// AgentProfileSpec 一次子 Agent 运行的角色规格（ADR-0103 决策三：Claude agents/*.md、Codex
// agents/*.toml 归一后的运行期形态）。由委派方解析后经 WithAgentProfile 注入内核；
// 只能收窄能力（工具白/黑名单、只读、步数），永不放宽宿主已有的安全边界。
type AgentProfileSpec struct {
	Name   string // 调用名：插件 agent 为 "<plugin>:<agent>"
	Source string // "plugin:<id>" / "project" / "user"
	// Instructions 角色指令（Claude 正文 / Codex developer_instructions，含预加载技能正文）。
	// 来自扩展或项目文件，按 InstructionTaint 写入可变技能区，不进入内核指令区。
	Instructions     string
	InstructionTaint TaintLevel
	// Tools 为空 = 继承父 Agent 全部工具（Claude 语义）；名称按 catalog.NewToolRestriction 解析。
	Tools           []string
	DisallowedTools []string
	ReadOnly        bool // Codex sandbox_mode = "read-only"：拒绝写文件/起进程/改状态的工具
	MaxTurns        int  // Claude maxTurns：>0 时收紧内核步数上限
	// AllowDelegation 子 Agent 能否继续委派。Claude 子 Agent 不能再派生子 Agent，
	// Codex agents.max_depth 默认 1——两家默认均为 false。
	AllowDelegation bool
}
