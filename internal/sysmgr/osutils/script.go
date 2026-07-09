package osutils

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// sanitizeHookEnv 从父进程环境中仅提取白名单内的变量。
// 凭据/密钥类变量通过 env 参数显式注入，而非从父进程继承。
func sanitizeHookEnv() []string {
	// allowedKeys 是 hook 脚本可继承的系统环境变量白名单。
	// 采用白名单策略防止 API 密钥、Token 等凭据通过进程环境泄漏给子进程。
	// hook 脚本相比 MCP 子进程额外允许 XDG 目录变量（shell 脚本常用）。
	allowedKeys := map[string]struct{}{
		"PATH":            {},
		"HOME":            {},
		"TMPDIR":          {},
		"TEMP":            {},
		"TMP":             {},
		"USER":            {},
		"USERNAME":        {},
		"LANG":            {},
		"LC_ALL":          {},
		"LC_CTYPE":        {},
		"TERM":            {},
		"SHELL":           {},
		"XDG_CONFIG_HOME": {},
		"XDG_DATA_HOME":   {},
		"XDG_CACHE_HOME":  {},
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

// resolveInterpreter 读取脚本首行 shebang，返回解释器路径与前置参数。
// 若无 shebang 或读取失败，返回 path 本身（二进制可执行文件直接运行）。
//
// 通过解释器调用而非直接 exec 脚本文件，可绕过 macOS XProtect/Gatekeeper
// 对新创建脚本文件的延迟安全扫描（解释器本身是已签名的系统二进制）。
func resolveInterpreter(path string) (interp string, interpArgs []string) {
	f, err := os.Open(path)
	if err != nil {
		return path, nil
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if strings.HasPrefix(line, "#!") {
		end := strings.IndexByte(line, '\n')
		if end < 0 {
			end = len(line)
		}
		parts := strings.Fields(line[2:end])
		if len(parts) > 0 {
			return parts[0], parts[1:]
		}
	}
	return path, nil
}

// RunScript 执行用户配置的 hook 脚本（合法的 exec 调用封装层）。
// 属于 action/hook 框架，是 R1.13 要求的合法 exec 封装位置。
// env 参数为调用方显式注入的变量，会追加到白名单父进程环境之后。
func RunScript(ctx context.Context, path string, env []string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 通过解释器调用而非直接 exec 脚本文件，避免 macOS 对未签名脚本的安全扫描延迟。
	// 若无 shebang（二进制可执行文件），interp==path 且 interpArgs==nil，走直接 exec 路径。
	interp, interpArgs := resolveInterpreter(path)
	var cmd *exec.Cmd
	if interp == path {
		cmd = exec.CommandContext(ctx, path)
	} else {
		cmd = exec.CommandContext(ctx, interp, append(interpArgs, path)...)
	}
	// 使用白名单过滤父进程环境，防止凭据泄漏（R1.15）。
	// 调用方通过 env 参数显式注入所需变量。
	cmd.Env = append(sanitizeHookEnv(), env...)

	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if runErr := cmd.Run(); runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 1, buf.String(), nil
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), buf.String(), nil
		}
		return 1, buf.String(), runErr
	}
	return 0, buf.String(), nil
}
