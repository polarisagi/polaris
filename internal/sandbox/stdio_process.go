package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// StdioRequest 一次性沙箱进程：stdin 注入、stdout/stderr 分离、保留退出码。
//
// 两家 hook 协议（Claude / Codex hooks.json）要求：输入 JSON 经 stdin 传入，退出码 2 表示
// 阻断且原因取 stderr，退出码 0 时 stdout 可为 JSON 决策。CmdRunner 的 run-to-completion
// 语义合并 stdout/stderr 且无 stdin，无法承载该协议；此处与 MCP stdio、L4 持久会话一样
// 经 ArgvWrapper 取得 Rust 沙箱封装后的 argv 自行启动进程（ADR-0103 决策六）。
type StdioRequest struct {
	CallerType   string   // protocol.SandboxCallerType
	Command      string   // shell 形式（bash -c）；与 ExecPath 二选一
	ExecPath     string   // exec 形式：可执行文件 + Args，不经 shell（参数中的变量值不会被 shell 再解析）
	Args         []string // exec 形式参数
	Stdin        []byte
	Env          []string // KEY=VALUE，叠加在沙箱 preset 之上
	WorkDir      string
	AllowedPaths []string
	AllowNet     bool
	Timeout      time.Duration
	MaxOutput    int // stdout / stderr 各自上限（字节）；0 = 1MiB
}

// StdioResult 进程结果。TimedOut 时 ExitCode 为 -1。
type StdioResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	TimedOut bool
	Method   string
	Duration time.Duration
}

var errStdioWrapperMissing = apperr.New(apperr.CodeForbidden, "stdio process: sandbox argv wrapper not configured (fail-closed)")

// RunStdio 在沙箱中执行进程。wrapper 为 nil 或封装失败时拒绝执行，不降级为裸 exec。
func RunStdio(ctx context.Context, wrapper ArgvWrapper, req StdioRequest) (StdioResult, error) {
	if wrapper == nil {
		return StdioResult{}, errStdioWrapperMissing
	}
	sctx := protocol.SandboxContext{
		CallerType:    req.CallerType,
		Workdir:       req.WorkDir,
		AllowedPaths:  req.AllowedPaths,
		EnvExtra:      req.Env,
		NetworkPolicy: protocol.NetPolicyDeny,
	}
	if req.AllowNet {
		sctx.NetworkPolicy = protocol.NetPolicyAllow
	}
	switch {
	case req.ExecPath != "":
		sctx.ExecPath, sctx.ExecArgs = req.ExecPath, req.Args
	case req.Command != "":
		sctx.ExecPath, sctx.ExecArgs = shellArgv(req.Command)
	default:
		return StdioResult{}, apperr.New(apperr.CodeInvalidInput, "stdio process: command or exec path required")
	}
	wrapped, err := wrapper.WrapArgv(ctx, sctx)
	if err != nil {
		return StdioResult{}, apperr.Wrap(apperr.CodeForbidden, "stdio process: sandbox wrap failed, refusing to spawn unsandboxed", err)
	}
	return runWrapped(ctx, wrapped, req)
}

func runWrapped(ctx context.Context, wrapped *protocol.WrapArgvResult, req StdioRequest) (StdioResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 600 * time.Second // 两家标准的命令 hook 默认超时
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limit := req.MaxOutput
	if limit <= 0 {
		limit = 1 << 20
	}
	cmd := exec.CommandContext(runCtx, wrapped.Executable, wrapped.Argv...) //nolint:gosec // argv 来自 Rust 沙箱封装
	cmd.SysProcAttr = setPdeathsig(cmd.SysProcAttr)
	cmd.Dir = req.WorkDir
	if !wrapped.EnvInArgv {
		cmd.Env = wrapped.Env
	} else {
		cmd.Env = []string{}
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	stdout, stderr := &limitedBuffer{max: limit}, &limitedBuffer{max: limit}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	runErr := cmd.Run()
	res := StdioResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Method: wrapped.SandboxMethod, Duration: time.Since(start)}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut, res.ExitCode = true, -1
		return res, nil
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, apperr.Wrap(apperr.CodeInternal, "stdio process: start failed", runErr)
	}
	return res, nil
}

func shellArgv(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "/bin/bash", []string{"-c", command}
}

// limitedBuffer 超出上限的输出静默截断，防止失控进程撑爆内存（Tier-0 2GB）。
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
