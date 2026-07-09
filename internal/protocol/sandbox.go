// Package protocol — sandbox.go
//
// SandboxContext V2 统一沙箱请求类型（Go mirror），对应 Rust SandboxContextV2。
// 所有沙箱调用方（builtin/mcp/codeact/skill/hook/plugin）使用同一结构体，
// 由调用方填写 CallerType，由 Rust 层推导 env_preset / 路径策略。
//
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2，ADR-0011

package protocol

// SandboxCallerType 调用方类型常量（驱动 env_preset 和路径策略）。
type SandboxCallerType = string

const (
	CallerBuiltin SandboxCallerType = "builtin"
	CallerMCP     SandboxCallerType = "mcp"
	CallerCodeAct SandboxCallerType = "codeact"
	CallerSkill   SandboxCallerType = "skill"
	CallerHook    SandboxCallerType = "hook"
	CallerPlugin  SandboxCallerType = "plugin"
)

// SandboxEnvPreset 环境注入预设（Rust 侧推导默认值，此处供调用方显式覆盖）。
type SandboxEnvPreset = string

const (
	// EnvPresetMinimal PATH/HOME/TMPDIR/LANG/TZ（MCP/Plugin 默认）
	EnvPresetMinimal SandboxEnvPreset = "minimal"
	// EnvPresetRuntime minimal + 语言运行时变量（builtin/codeact/skill 默认）
	EnvPresetRuntime SandboxEnvPreset = "runtime"
	// EnvPresetPassthroughSafe runtime + 代理/编辑器等安全变量透传
	EnvPresetPassthroughSafe SandboxEnvPreset = "passthrough_safe"
)

// SandboxNetworkPolicy 网络访问策略。
type SandboxNetworkPolicy = string

const (
	// NetPolicyDeny 禁止所有出站网络（默认）
	NetPolicyDeny SandboxNetworkPolicy = "deny"
	// NetPolicyDomainWhitelist 仅允许 NetworkDomains 中列出的域名
	// macOS：Seatbelt SBPL 端口规则；Linux：降级 allow（宿主防火墙负责 DNS 过滤）
	NetPolicyDomainWhitelist SandboxNetworkPolicy = "domain_whitelist"
	// NetPolicyAllow 允许所有出站网络
	NetPolicyAllow SandboxNetworkPolicy = "allow"
)

// SandboxContext V2 统一沙箱请求（JSON 序列化后传入 Rust FFI）。
// 字段均为指针/切片以支持 omitempty，Rust 侧对 None 字段有合理默认值。
type SandboxContext struct {
	// CallerType 驱动 env preset 和路径策略（必填）
	CallerType string `json:"caller_type,omitempty"`

	// Command shell 命令（bash -c 包裹），与 ExecPath 二选一
	Command string `json:"command,omitempty"`
	// ExecPath 直接执行路径（MCP/wrap_argv 模式，不经 bash -c 包裹）
	ExecPath string `json:"exec_path,omitempty"`
	// ExecArgs 直接执行参数（与 ExecPath 配合）
	ExecArgs []string `json:"exec_args,omitempty"`

	// Workdir 工作目录（默认 /tmp）
	Workdir string `json:"workdir,omitempty"`
	// AllowedPaths 可读写路径白名单（workspace / 项目目录）
	AllowedPaths []string `json:"allowed_paths,omitempty"`

	// EnvPreset 环境注入预设（空字符串 = 由 CallerType 推导）
	EnvPreset string `json:"env_preset,omitempty"`
	// EnvExtra 额外注入 KEY=VALUE（凭据过滤后追加/覆盖 preset）
	EnvExtra []string `json:"env_extra,omitempty"`

	// NetworkPolicy 网络策略（默认 "deny"）
	NetworkPolicy string `json:"network_policy,omitempty"`
	// NetworkDomains domain_whitelist 时允许的域名列表
	NetworkDomains []string `json:"network_domains,omitempty"`

	// BindHostTmp true = bwrap 用 --bind /tmp /tmp（CodeAct 脚本在 host /tmp）
	BindHostTmp bool `json:"bind_host_tmp,omitempty"`
	// ScriptPath 单文件显式绑定（如 CodeAct 临时脚本绝对路径）
	ScriptPath string `json:"script_path,omitempty"`

	// TimeoutMs 超时毫秒（默认 30000）
	TimeoutMs uint64 `json:"timeout_ms,omitempty"`
	// BwrapPath Linux bwrap 路径覆盖（空 = 自动查找）
	BwrapPath string `json:"bwrap_path,omitempty"`
	// MaxMemoryMB 内存限制 MB（0 = 不限）
	MaxMemoryMB uint64 `json:"max_memory_mb,omitempty"`
}

// WrapArgvResult native_sandbox_wrap_argv 响应（Go 侧用于构建 exec.Cmd）。
type WrapArgvResult struct {
	// Executable 可执行文件绝对路径
	Executable string `json:"executable"`
	// Argv 参数列表（不含 Executable 自身）
	Argv []string `json:"argv"`
	// Env KEY=VALUE 列表（EnvInArgv=true 时为空，bwrap 侧 env 已嵌入 Argv）
	Env []string `json:"env"`
	// EnvInArgv true = env 已通过 --setenv 嵌入 Argv（bwrap）
	EnvInArgv bool `json:"env_in_argv"`
	// SandboxMethod 实际使用的沙箱方法
	SandboxMethod string `json:"sandbox_method"`
	// NetIsolated 网络是否被隔离
	NetIsolated bool `json:"net_isolated"`
}
