package mcp

import (
	"os"
	"strings"
)

// sanitizeParentEnv 仅将白名单内的无害系统变量传递给 MCP 子进程。
// 采用白名单策略：未显式授权的变量一律拦截，防止凭据通过非典型键名泄漏。
func sanitizeParentEnv() []string {
	// allowedEnvKeys 是 MCP 子进程可继承的环境变量白名单（全大写精确匹配）。
	// 凭据/密钥类变量需通过显式注入（not os.Environ），防止黑名单绕过。
	// MCP 子进程可能是 Rust/Python/Go/Node 编写的服务端，
	// 必须透传语言运行时所需的工具链路径变量，否则命令找不到解释器/标准库。
	allowedEnvKeys := map[string]struct{}{
		// 基础系统
		"PATH":     {},
		"HOME":     {},
		"TMPDIR":   {},
		"TEMP":     {},
		"TMP":      {},
		"USER":     {},
		"USERNAME": {},
		"LANG":     {},
		"LC_ALL":   {},
		"LC_CTYPE": {},
		"TERM":     {},
		"SHELL":    {},
		// Go 运行时
		"GOPATH":     {},
		"GOROOT":     {},
		"GOMODCACHE": {},
		"GOCACHE":    {},
		// Rust 工具链
		"CARGO_HOME":  {},
		"RUSTUP_HOME": {},
		// Python（含 pyenv / conda / uv 路径；子进程解释器解析依赖这些变量）
		"PYTHONPATH":              {},
		"PYTHONDONTWRITEBYTECODE": {},
		"VIRTUAL_ENV":             {},
		"PYENV_ROOT":              {},
		"PYENV_VERSION":           {},
		"CONDA_PREFIX":            {},
		"CONDA_DEFAULT_ENV":       {},
		// Node（含 nvm / mise / asdf 版本管理器路径）
		"NODE_PATH": {},
		"NODE_ENV":  {},
		"NVM_DIR":   {},
		"NVM_BIN":   {},
		// 通用版本管理器
		"ASDF_DIR":        {},
		"ASDF_DATA_DIR":   {},
		"MISE_ROOT":       {},
		"XDG_RUNTIME_DIR": {},
		"XDG_CONFIG_HOME": {},
		"XDG_DATA_HOME":   {},
		// Java
		"JAVA_HOME": {},
	}
	raw := os.Environ()
	out := make([]string, 0, len(allowedEnvKeys))
	for _, kv := range raw {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := strings.ToUpper(kv[:idx])
		if _, ok := allowedEnvKeys[key]; ok {
			out = append(out, kv)
		}
	}
	return out
}
