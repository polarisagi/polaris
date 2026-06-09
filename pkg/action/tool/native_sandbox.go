package tool

// native_sandbox.go — 平台原生进程沙箱封装
//
// 架构对齐：
//   macOS  → Apple Seatbelt（sandbox-exec，内置，无需安装）
//   Linux  → bubblewrap（bwrap，需 apt/yum 安装；不可用时降级 namespace）
//   Windows→ WSL2（wsl.exe，提供 VM 级隔离；不可用时降级 fallback）
//
// 参照：Claude Code sandboxing（anthropic.com/engineering/claude-code-sandboxing）
//       Codex CLI sandbox modes（developers.openai.com/codex/concepts/sandboxing）

import (
	"context"
	"fmt"
	"log/slog"
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
}

// WrapBashCmd 将 bash 命令包装到平台原生沙箱，返回可执行的 exec.Cmd。
// 调用方不应修改返回的 Cmd（Dir/Env/SysProcAttr 已设置）。
func WrapBashCmd(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
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

// wrapDarwin 使用 Apple Seatbelt 隔离子进程。
// sandbox-exec 是 macOS 内置工具（macOS 10.5+），无需安装任何依赖。
// 安全边界：读取限于系统目录；写入限于 AllowedPaths + /tmp；网络受 NetworkPolicy 控制。
func wrapDarwin(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		slog.Warn("native_sandbox: sandbox-exec not found on macOS, falling back to minimal isolation")
		return wrapFallback(ctx, cfg)
	}
	profile := buildSeatbeltProfile(cfg)
	// -p 接收内联策略字符串，避免临时文件竞争
	cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", profile, "bash", "-c", cfg.Command)
	cmd.Dir = cfg.WorkDir
	cmd.Env = cfg.Env
	return cmd, nil
}

// buildSeatbeltProfile 构建 SBPL（Sandbox Profile Language）策略。
// 策略原则：默认拒绝，白名单开放。
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
; 系统目录只读（编译器/解释器/标准库）
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
`)
	// Workspace 路径可读写
	for _, p := range cfg.AllowedPaths {
		fmt.Fprintf(&sb, "(allow file* (subpath %q))\n", sbplEscape(p))
	}
	if cfg.WorkDir != "" {
		fmt.Fprintf(&sb, "(allow file* (subpath %q))\n", sbplEscape(cfg.WorkDir))
	}

	// 网络策略
	switch cfg.NetworkPolicy {
	case NetworkAllow:
		sb.WriteString("; 允许所有出站网络\n(allow network*)\n")
	default: // NetworkBlock
		sb.WriteString("; 禁止所有网络（默认，对齐 Claude Code 行为）\n(deny network*)\n")
	}

	return sb.String()
}

// sbplEscape 对路径进行 SBPL 安全转义（仅处理双引号和反斜杠）。
func sbplEscape(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	path = strings.ReplaceAll(path, `"`, `\"`)
	return path
}

// ── Linux: bubblewrap（bwrap）────────────────────────────────────────────────

// wrapLinux 优先使用 bubblewrap 提供文件系统 + 网络双重隔离；
// 不可用时降级到 namespace-only（CLONE_NEWPID+CLONE_NEWNS）。
// 安装：sudo apt-get install bubblewrap / sudo dnf install bubblewrap
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

// wrapWithBwrap 构建 bubblewrap 命令。
// 隔离层：PID / UTS / IPC namespace + 只读系统 + 可写 workspace + 可选网络隔离。
func wrapWithBwrap(ctx context.Context, bwrapPath string, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	args := []string{
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
		// 系统目录只读绑定
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/lib32", "/lib32",
		"--ro-bind-try", "/etc", "/etc",
		"--ro-bind-try", "/opt", "/opt",
		"--ro-bind-try", "/nix", "/nix",
		// 必要的虚拟文件系统
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
	}

	// 网络隔离（对齐 Claude Code 默认全禁）
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

	// 环境变量注入（bwrap 默认清空，需显式传入）
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

// wrapWindows 通过 WSL2 执行 bash 命令。
// WSL2 本质是 Hyper-V 轻量 VM，提供 VM 级进程隔离；bash 在 Linux VM 内执行。
// 约束：需要 WSL2 已安装且有默认 distro；--cd 将工作目录映射到 Windows 路径。
func wrapWindows(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		slog.Warn("native_sandbox: WSL2 not found on Windows, falling back to minimal isolation",
			"tip", "install WSL2: https://aka.ms/wsl2")
		return wrapFallback(ctx, cfg)
	}

	args := []string{}
	if cfg.WorkDir != "" {
		// --cd 接受 Windows 路径（bwrap 在 WSL2 内部处理）
		args = append(args, "--cd", cfg.WorkDir)
	}
	args = append(args, "-e", "bash", "-c", cfg.Command)
	cmd := exec.CommandContext(ctx, "wsl.exe", args...)
	// 网络策略在 WSL2 层无法直接控制；依赖 Windows 防火墙规则
	if cfg.NetworkPolicy == NetworkBlock {
		slog.Warn("native_sandbox: network blocking not enforced inside WSL2; configure Windows Firewall manually")
	}
	return cmd, nil
}

// ── 降级路径 ───────────────────────────────────────────────────────────────────

// wrapFallback 无平台沙箱时的最小安全实现：清理环境变量 + workDir + namespace（Linux）。
// 调用方应通过日志警告告知操作员此路径不安全。
func wrapFallback(ctx context.Context, cfg NativeSandboxCfg) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", cfg.Command)
	cmd.Dir = cfg.WorkDir
	cmd.Env = cfg.Env
	// Linux：注入 namespace 作为最后防线（非 Linux 时 SysProcAttr=nil，跳过）
	// 这复用了 pkg/action/sandbox_linux.go 的 ContainerSandboxSysProcAttr()
	// 但此处不能直接调用（跨包），改为在 builtin_tools.go 中注入
	return cmd, nil
}
