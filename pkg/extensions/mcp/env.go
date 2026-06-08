package mcp

import (
	"os"
	"strings"
)

// allowedEnvKeys 是 MCP 子进程可继承的环境变量白名单（全大写精确匹配）。
// 凭据/密钥类变量需通过显式注入（not os.Environ），防止黑名单绕过。
var allowedEnvKeys = map[string]struct{}{
	"PATH":      {},
	"HOME":      {},
	"TMPDIR":    {},
	"TEMP":      {},
	"TMP":       {},
	"USER":      {},
	"USERNAME":  {},
	"LANG":      {},
	"LC_ALL":    {},
	"LC_CTYPE":  {},
	"TERM":      {},
	"SHELL":     {},
	"NODE_PATH": {},
	"GOPATH":    {},
	"GOROOT":    {},
}

// sanitizeParentEnv 仅将白名单内的无害系统变量传递给 MCP 子进程。
// 采用白名单策略：未显式授权的变量一律拦截，防止凭据通过非典型键名泄漏。
func sanitizeParentEnv() []string {
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
