package sandbox

import (
	"context"
	"os"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// sandboxMinEnv 构造容器沙箱测试进程所需的最小环境变量集合。
// 凭据和业务 key 通过 proxy.EnvVars() 显式注入，不从父进程继承（R1.15）。
func sandboxMinEnv() []string {
	// allowedKeys 是容器沙箱（DryRunMode）子进程可继承的最小环境变量白名单。
	// DryRunMode 仅用于测试，沙箱二进制只需基础运行时变量 + mock proxy 注入的变量（R1.15）。
	allowedKeys := map[string]struct{}{
		"PATH":     {},
		"HOME":     {},
		"TMPDIR":   {},
		"TEMP":     {},
		"TMP":      {},
		"USER":     {},
		"USERNAME": {},
		"LANG":     {},
		"LC_ALL":   {},
		"GOPATH":   {},
		"GOROOT":   {},
	}
	raw := os.Environ()
	out := make([]string, 0, len(allowedKeys))
	for _, kv := range raw {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := strings.ToUpper(kv[:idx])
		if _, ok := allowedKeys[key]; ok {
			out = append(out, kv)
		}
	}
	return out
}

// containerBaseEnv 生产沙箱进程所需的安全基础环境变量集合。
// 比 sandboxMinEnv 多透传语言运行时变量，适用于 CodeAct / 技能脚本执行。
// 白名单策略：明确列举的键才传入，凭据/密钥类键一律拦截（R1.15）。
func containerBaseEnv() []string {
	allowedKeys := map[string]struct{}{
		"PATH":                    {},
		"HOME":                    {},
		"TMPDIR":                  {},
		"TEMP":                    {},
		"TMP":                     {},
		"USER":                    {},
		"USERNAME":                {},
		"LANG":                    {},
		"LC_ALL":                  {},
		"LC_CTYPE":                {},
		"GOPATH":                  {},
		"GOROOT":                  {},
		"GOMODCACHE":              {},
		"GOCACHE":                 {},
		"CARGO_HOME":              {},
		"RUSTUP_HOME":             {},
		"PYTHONPATH":              {},
		"PYTHONDONTWRITEBYTECODE": {},
		"VIRTUAL_ENV":             {},
		"NODE_PATH":               {},
		"NODE_ENV":                {},
		"JAVA_HOME":               {},
	}
	raw := os.Environ()
	out := make([]string, 0, 24)
	for _, kv := range raw {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		if _, ok := allowedKeys[strings.ToUpper(kv[:idx])]; ok {
			out = append(out, kv)
		}
	}
	return out
}

// SandboxProvider 是沙箱执行抽象接口，允许对 InProcess/Container 分别实现。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2
type SandboxProvider interface {
	// Run 执行工具并返回结果。spec 描述执行约束。
	Run(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error)
}

// SandboxSpec 描述一次沙箱执行的完整规格。
type SandboxSpec struct {
	ToolName     string
	Input        []byte
	SandboxTier  types.SandboxTier
	Capability   types.CapabilityLevel
	SideEffects  []types.SideEffect
	ScriptPath   string   // TypeScript/Python 脚本路径（L3 Container 执行时使用）
	ScriptBytes  []byte   // 脚本源码（测试或直接下发时使用）
	Command      string   // 任意 shell 命令字符串（bash -c 语义），当前仅 Hook 引擎使用；与 ScriptPath 互斥，ScriptPath 优先
	AllowedPaths []string // 文件系统白名单
	ExtraEnv     []string // 追加环境变量（叠加在 containerBaseEnv() 之后），当前仅 Hook 引擎传 HOOK_INPUT_JSON 使用
	CPUQuotaMs   int      // 0 = 默认 5000ms
	IOBudget     int64    // 0 = 默认 8MB
	MaxCalls     int      // 0 = 默认 10000
	SystemTier   int      // 硬件分级
	TaintLevel   types.TaintLevel
	DryRunMode   bool
	MockProxyEnv string
}
