package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// liveSession 是一个长驻解释器进程（D4/ADR-0079）。与一次性沙箱执行不同，
// 这里的进程在多次 PersistentSandbox.Run 调用之间保持存活，语言运行时状态
// （变量/导入/打开的文件句柄等）天然保留在进程内存中，不需要 pickle/env 序列化。
//
// 并发模型：同一 liveSession 同一时刻只能处理一次 exec 调用（单个解释器无法
// 并行执行两段代码），execMu 串行化；跨 session 并发不受影响。
type liveSession struct {
	sessionID string
	language  string // "python" | "bash"
	workDir   string // 会话专属工作目录，kill 时一并清理

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	pr     *os.File // stdout+stderr 合并管道的读端（父进程持有）

	sentinel string // bash 协议哨兵 token（每会话一个 UUID，降低与真实输出撞行的概率）

	execMu sync.Mutex
	dead   atomic.Bool

	lastUsedNano atomic.Int64
	createdAt    time.Time
}

func (s *liveSession) touch() {
	s.lastUsedNano.Store(time.Now().UnixNano())
}

func (s *liveSession) idleSince() time.Duration {
	return time.Since(time.Unix(0, s.lastUsedNano.Load()))
}

// alive 是否仍可能存活（best-effort，不保证下一次写入一定成功——进程可能在
// 检查后、写入前才退出，exec() 内部的写失败/超时是最终防线）。
func (s *liveSession) alive() bool {
	return !s.dead.Load()
}

// kill 终止会话进程并释放所有资源。幂等（重复调用安全）。
func (s *liveSession) kill() {
	if s.dead.Swap(true) {
		return
	}
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_, _ = s.cmd.Process.Wait() //nolint:errcheck // 回收僵尸进程，退出状态不关心
	_ = s.pr.Close()
	if s.workDir != "" {
		_ = os.RemoveAll(s.workDir)
	}
}

type sessionExecResult struct {
	ok        bool
	output    string
	errorText string
}

// exec 向长驻进程发送一段代码并等待其执行结果。任何协议层面的失败（写入失败/
// 读取失败/响应格式错误/超时）都被视为会话不可再信任——调用方（PersistentSandbox.Run）
// 负责在收到 error 后从注册表移除并 kill 该会话，下一次调用会拿到全新会话，
// 不会让一个协议已经"错位"的旧会话带病继续使用。
func (s *liveSession) exec(ctx context.Context, code string) (*sessionExecResult, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	if s.dead.Load() {
		return nil, apperr.New(apperr.CodeInternal, "sandbox: L4 session already terminated")
	}

	var result *sessionExecResult
	var err error
	switch s.language {
	case "python":
		result, err = s.execPython(ctx, code)
	case "bash":
		result, err = s.execBash(ctx, code)
	default:
		err = apperr.New(apperr.CodeInternal, "sandbox: L4 unknown session language "+s.language)
	}
	if err != nil {
		s.kill()
		return nil, err
	}
	s.touch()
	return result, nil
}

// pythonHarnessResponse 与 pythonSessionHarness 写回的 JSON 结构对齐。
type pythonHarnessResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

func (s *liveSession) execPython(ctx context.Context, code string) (*sessionExecResult, error) {
	task, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 python: marshal task", err)
	}
	if _, err := s.stdin.Write(append(task, '\n')); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 python: write stdin failed", err)
	}

	type readOutcome struct {
		line string
		err  error
	}
	ch := make(chan readOutcome, 1)
	// 用 SafeGo 而非裸 goroutine（inv_NoBareGoroutine）：即便阻塞在 ReadString
	// 上，也要保证 panic 不会静默吞掉。ctx 用 WithoutCancel——这个 goroutine
	// 的生命周期不由 exec() 的 ctx 直接控制（select 分支超时后 goroutine 仍会
	// 继续阻塞直到 kill() 关闭底层管道才解除阻塞，见文件头协议说明），传入
	// WithoutCancel 只是满足 SafeGo 签名，不代表这里有真实的取消语义。
	concurrent.SafeGo(context.WithoutCancel(ctx), "sandbox_persistent.python_read", func(context.Context) {
		line, err := s.stdout.ReadString('\n')
		ch <- readOutcome{line: line, err: err}
	})

	select {
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeInternal, "sandbox: L4 python: exec timed out or canceled")
	case r := <-ch:
		if r.err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 python: read stdout failed", r.err)
		}
		var resp pythonHarnessResponse
		if err := json.Unmarshal([]byte(r.line), &resp); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 python: malformed harness response: "+r.line, err)
		}
		out := resp.Output
		if resp.Stderr != "" {
			if out != "" {
				out += "\n"
			}
			out += resp.Stderr
		}
		return &sessionExecResult{ok: resp.OK, output: out, errorText: resp.Error}, nil
	}
}

// execBash 把代码文本 + 一行哨兵 echo 写入长驻 `bash -s` 进程的 stdin，读取
// stdout（stderr 已在 spawn 时合并进同一管道）直到命中哨兵行为止，从中解析出
// 上一条命令链的退出码。
func (s *liveSession) execBash(ctx context.Context, code string) (*sessionExecResult, error) {
	marker := "<<<POLARIS_END:" + s.sentinel + ":"
	script := code
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	script += "echo \"" + marker + "$?>>>\"\n"

	if _, err := s.stdin.Write([]byte(script)); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 bash: write stdin failed", err)
	}

	type readOutcome struct {
		output   string
		exitCode int
		err      error
	}
	ch := make(chan readOutcome, 1)
	// 同上（execPython）：SafeGo 包裹 + WithoutCancel，理由一致。
	concurrent.SafeGo(context.WithoutCancel(ctx), "sandbox_persistent.bash_read", func(context.Context) {
		var sb strings.Builder
		for {
			line, err := s.stdout.ReadString('\n')
			if idx := strings.Index(line, marker); idx >= 0 {
				rest := strings.TrimSuffix(strings.TrimRight(line[idx+len(marker):], "\r\n"), ">>>")
				code, convErr := strconv.Atoi(rest)
				if convErr != nil {
					code = -1
				}
				ch <- readOutcome{output: sb.String(), exitCode: code}
				return
			}
			sb.WriteString(line)
			if err != nil {
				ch <- readOutcome{err: err}
				return
			}
		}
	})

	select {
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeInternal, "sandbox: L4 bash: exec timed out or canceled")
	case r := <-ch:
		if r.err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 bash: read stdout failed", r.err)
		}
		return &sessionExecResult{ok: r.exitCode == 0, output: r.output}, nil
	}
}
