// Package tool — native_sandbox.go
//
// 平台原生进程沙箱封装。
//
// 架构：优先调用 Rust FFI（native_sandbox_exec），Rust dylib 不可用时降级为 Go 本地实现。
//   Rust 实现（优先）: macOS Seatbelt / Linux bwrap / Windows WSL2 + 自动 PATH 探测
//   Go 降级（备用）  : 同平台逻辑，用于 dylib 未构建的开发环境
//
// 参照：Claude Code sandboxing（anthropic.com/engineering/claude-code-sandboxing）
//       Codex CLI sandbox modes（developers.openai.com/codex/concepts/sandboxing）

package tool

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// NetworkPolicy 网络访问策略。
type NetworkPolicy string

const (
	// NetworkBlock 禁止所有出站网络（默认，对齐 Claude Code / Codex CLI）。
	NetworkBlock NetworkPolicy = "block"
	// NetworkAllow 允许所有出站网络（宽松模式）。
	NetworkAllow NetworkPolicy = "allow"
)

// NativeSandboxCfg 单次沙箱调用配置（由 makeBashFn / makeRunCommandFn 构造）。
type NativeSandboxCfg struct {
	Command       string        // 待执行的 shell 命令
	WorkDir       string        // 工作目录（白名单中的首个路径）
	AllowedPaths  []string      // 可读写路径白名单（workspace 范围）
	NetworkPolicy NetworkPolicy // 网络策略
	Env           []string      // 环境变量（已清理）
	BwrapPath     string        // Linux: bwrap 可执行路径（空=自动查找）
	TimeoutMs     uint64        // 超时毫秒（0 = Rust 侧默认 30000）
	MaxMemoryMB   uint64        // 内存限制（MB，0=不限制）
}

// WrapBashCmd 将 bash 命令包装到平台原生沙箱，返回可执行的 exec.Cmd。
//
// 优先调用 Rust FFI 直接执行（无需返回 Cmd），若 Rust 路径不可用则构造 Go exec.Cmd
// 交由调用方执行。因此返回值分两种情况：
//
//	result != nil  → Rust 已执行完毕，Cmd 为 nil（调用方直接使用 result）
//	result == nil  → 返回 Go exec.Cmd，调用方自行 cmd.CombinedOutput()
//
// 注意：调用方需检查 result 而非总是用 cmd。
func WrapBashCmd(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, *RustSandboxResponse, error) {
	// ── 优先路径：Rust FFI ────────────────────────────────────────────────
	timeoutMs := cfg.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = 30_000
	}

	resp, err := RustSandboxExec(cfg, timeoutMs)
	if err == nil {
		return nil, resp, nil
	}

	// Rust dylib 未加载或不可用 → 降级到 Go 实现，记录警告
	slog.Warn("native_sandbox: Rust FFI unavailable, falling back to Go implementation",
		"error", err,
		"tip", "run `make rust-build` to build the substrate dylib")

	// ── 降级路径：Go 本地实现 ─────────────────────────────────────────────
	goCmd, goErr := wrapBashCmdGo(ctx, cfg)
	if goErr != nil {
		return nil, nil, goErr
	}
	return goCmd, nil, nil
}

// wrapBashCmdGo Go 本地沙箱实现（降级路径）。
// 与 Rust 实现对应：macOS Seatbelt / Linux bwrap / Windows WSL2。
func wrapBashCmdGo(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		return wrapDarwin(ctx, cfg)
	case "linux":
		return wrapLinux(ctx, cfg)
	case "windows":
		return wrapWindows(ctx, cfg)
	default:
		slog.Warn("native_sandbox: unknown platform, no isolation", "goos", runtime.GOOS)
		return wrapFallback(ctx, cfg)
	}
}

// ── macOS: Seatbelt（sandbox-exec）────────────────────────────────────────────

func wrapDarwin(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		slog.Warn("native_sandbox: sandbox-exec not found on macOS, falling back to minimal isolation")
		return wrapFallback(ctx, cfg)
	}
	profile := buildSeatbeltProfile(cfg)
	// 注入 PATH 前缀（sandbox-exec 会清空部分 env，确保工具可找到）
	envPrefix := buildEnvPrefix(cfg.Env)
	fullCmd := envPrefix + cfg.Command
	cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", profile, "bash", "-c", fullCmd)
	cmd.Dir = cfg.WorkDir
	cmd.Env = cfg.Env
	return cmd, nil
}

// buildSeatbeltProfile 构建 SBPL 策略（与 Rust 侧 build_seatbelt_profile 对齐）。
func buildSeatbeltProfile(cfg NativeSandboxCfg) string {
	var sb strings.Builder
	sb.WriteString(`(version 1)
(deny default)
(allow process-exec)
(allow process-fork)
(allow signal (target self))
(allow sysctl-read)
(allow ipc-posix*)
(allow mach-lookup)
(allow mach-register)
; 系统目录只读（编译器/解释器/标准库/Homebrew）
(allow file-read*
  (subpath "/usr")
  (subpath "/bin")
  (subpath "/sbin")
  (subpath "/System")
  (subpath "/Library")
  (subpath "/private/etc")
  (subpath "/private/var/db")
  (subpath "/opt")
  (subpath "/nix")
  (subpath "/Applications")
)
(allow file-read-metadata)
; /tmp 可读写（编译器缓存等）
(allow file* (subpath "/tmp"))
(allow file* (subpath "/private/tmp"))
(allow file* (subpath "/var/folders"))
(allow file* (subpath "/private/var/folders"))
`)
	// Workspace 路径可读写
	for _, p := range cfg.AllowedPaths {
		fmt.Fprintf(&sb, "(allow file* (subpath %s))\n", sbplEscape(p))
	}
	if cfg.WorkDir != "" {
		fmt.Fprintf(&sb, "(allow file* (subpath %s))\n", sbplEscape(cfg.WorkDir))
	}
	// 用户工具目录只读（cargo/pyenv/nvm/etc.）
	if home, err := os.UserHomeDir(); err == nil {
		toolDirs := []string{
			home + "/.cargo", home + "/.pyenv", home + "/.nvm",
			home + "/.local", home + "/go", home + "/.deno",
			home + "/.bun", home + "/.asdf", home + "/.rye",
		}
		for _, d := range toolDirs {
			if _, err := os.Stat(d); err == nil {
				fmt.Fprintf(&sb, "(allow file-read* (subpath %s))\n", sbplEscape(d))
			}
		}
	}
	// 网络策略
	switch cfg.NetworkPolicy {
	case NetworkAllow:
		sb.WriteString("; 允许所有出站网络\n(allow network*)\n")
	default:
		sb.WriteString("; 禁止所有网络（默认，对齐 Claude Code 行为）\n(deny network*)\n")
	}
	return sb.String()
}

// sbplEscape 将路径编码为 SBPL 双引号字符串字面量（含外层引号）。
// 例：/Users/foo bar → "/Users/foo bar"
// 调用方用 %s（不要用 %q，%q 会再加一层转义）。
func sbplEscape(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	path = strings.ReplaceAll(path, `"`, `\"`)
	return `"` + path + `"`
}

// ── Linux: bubblewrap（bwrap）────────────────────────────────────────────────

func wrapLinux(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	bwrapPath := cfg.BwrapPath
	if bwrapPath == "" {
		var err error
		bwrapPath, err = exec.LookPath("bwrap")
		if err != nil {
			slog.Warn("native_sandbox: bwrap not found; install bubblewrap for full sandbox",
				"tip", "sudo apt-get install bubblewrap")
			return wrapFallback(ctx, cfg)
		}
	}
	return wrapWithBwrap(ctx, bwrapPath, cfg)
}

func wrapWithBwrap(ctx context.Context, bwrapPath string, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	args := []string{
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
		// 系统目录只读绑定（保证语言运行时可访问）
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/lib32", "/lib32",
		"--ro-bind-try", "/etc", "/etc",
		"--ro-bind-try", "/opt", "/opt",
		"--ro-bind-try", "/nix", "/nix",
		"--ro-bind-try", "/snap", "/snap",
		// 必要的虚拟文件系统
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
	}

	// 用户工具目录只读绑定（解决 "command not found" 的核心修复）
	if home, err := os.UserHomeDir(); err == nil {
		toolDirs := []string{
			home + "/.cargo", home + "/.rustup",
			home + "/.pyenv", home + "/.nvm",
			home + "/.local", home + "/go", home + "/.go",
			home + "/.deno", home + "/.bun",
			home + "/.asdf", home + "/.rye",
			home + "/.local/share/mise",
			home + "/.rbenv",
		}
		for _, d := range toolDirs {
			if _, err := os.Stat(d); err == nil {
				args = append(args, "--ro-bind-try", d, d)
			}
		}
	}

	// 网络隔离
	if cfg.NetworkPolicy == NetworkBlock {
		args = append(args, "--unshare-net")
	}

	// Workspace 路径可读写绑定
	seen := map[string]bool{}
	for _, p := range cfg.AllowedPaths {
		if !seen[p] {
			args = append(args, "--bind-try", p, p)
			seen[p] = true
		}
	}
	if cfg.WorkDir != "" && !seen[cfg.WorkDir] {
		args = append(args, "--bind-try", cfg.WorkDir, cfg.WorkDir)
	}
	if cfg.WorkDir != "" {
		args = append(args, "--chdir", cfg.WorkDir)
	}

	// 环境变量注入（bwrap 默认清空所有 env，必须显式 --setenv）
	for _, e := range cfg.Env {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) == 2 {
			args = append(args, "--setenv", kv[0], kv[1])
		}
	}

	args = append(args, "--", "bash", "-c", cfg.Command)
	cmd := exec.CommandContext(ctx, bwrapPath, args...)
	return cmd, nil
}

// ── Windows: WSL2 ─────────────────────────────────────────────────────────────

func wrapWindows(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		slog.Warn("native_sandbox: WSL2 not found on Windows, falling back to minimal isolation",
			"tip", "install WSL2: https://aka.ms/wsl2")
		return wrapFallback(ctx, cfg)
	}

	args := []string{}
	if cfg.WorkDir != "" {
		// 将 Windows 路径转为 WSL2 /mnt/ 路径
		wslDir := windowsPathToWSL(cfg.WorkDir)
		if wslDir != "" {
			args = append(args, "--cd", wslDir)
		}
	}
	if cfg.NetworkPolicy == NetworkBlock {
		slog.Warn("native_sandbox: network blocking not enforced inside WSL2; configure Windows Firewall manually")
	}

	// PATH 注入前缀（WSL2 bash 继承 distro env，追加即可）
	envPrefix := buildEnvPrefix(cfg.Env)
	fullCmd := envPrefix + cfg.Command
	args = append(args, "-e", "bash", "-c", fullCmd)

	cmd := exec.CommandContext(ctx, "wsl.exe", args...)
	return cmd, nil
}

// windowsPathToWSL 将 Windows 路径转为 WSL2 /mnt/ 路径（C:\foo → /mnt/c/foo）。
func windowsPathToWSL(path string) string {
	if len(path) >= 3 && path[1] == ':' {
		drive := strings.ToLower(string(path[0]))
		rest := strings.ReplaceAll(path[2:], `\`, "/")
		return fmt.Sprintf("/mnt/%s%s", drive, rest)
	}
	// UNC 路径直接替换反斜杠
	return strings.ReplaceAll(path, `\`, "/")
}

// ── 降级路径 ───────────────────────────────────────────────────────────────────

func wrapFallback(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", cfg.Command)
	cmd.Dir = cfg.WorkDir
	cmd.Env = cfg.Env
	return cmd, nil
}

// ─── 工具函数 ─────────────────────────────────────────────────────────────────

// buildEnvPrefix 将 KEY=VALUE 列表转为 "export KEY=VALUE;" 前缀字符串，
// 注入到 bash -c 命令前确保工具可被找到（用于 sandbox-exec/WSL2 等继承 env 的场景）。
func buildEnvPrefix(envVars []string) string {
	if len(envVars) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, kv := range envVars {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
			// 单引号包裹 value（防 shell 展开，内部 ' 转义）
			safeV := strings.ReplaceAll(v, "'", "'\\''")
			fmt.Fprintf(&sb, "export %s='%s'; ", k, safeV)
		}
	}
	return sb.String()
}
