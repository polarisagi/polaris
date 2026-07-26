package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// PersistentSandbox 是 D4（原 GD-14-003，ADR-0079 推翻 ADR-0078 的"诚实留空"
// 结论）Sandbox-L4-Persistent 的真实实现：session-scoped 长驻解释器进程池，
// 而非 CRIU/Firecracker 式 checkpoint/restore。
//
// 背景与设计取舍详见 docs/arch/decisions/ADR-0079-sandbox-l4-live-process-pool.md，
// 核心结论：原始设计目标是"长程有状态 CodeAct 会话的状态不因每次调用重新起
// 进程而丢失"；CRIU/Firecracker 是达成这个目标的一种（在本仓库不可行的）手段，
// 不是目标本身。让解释器进程在多次调用之间根本不退出，同样能达成目标，且
// 不需要操作系统级 checkpoint/restore 原语、不需要引入 bwrap/Seatbelt 都不支持
// 的机制——文件句柄/线程/DB 连接等 pickle 无法序列化的状态，只要进程没死，
// 天然原样存在。
//
// 隔离强度：会话进程仍通过 ArgvWrapper（Rust FFI native_sandbox_wrap_argv，
// 与 L3/MCP stdio 长进程同一底层机制）取得 bwrap/Seatbelt 封装后的 argv/env
// 再由 Go 侧启动并持有管道，不是裸 exec.Command——与 CodeAct today 通过 L2/L3
// 得到的隔离强度一致，不构成 inv_global_07"禁止降级隔离"的例外。
//
// 已知边界（如实记录，非本实现遗漏）：
//   - 沙箱边界（AllowedPaths/网络策略）在会话首次创建时固化，同一 SessionID
//     后续调用无法更改（bwrap/Seatbelt profile 在进程启动时确定，长驻进程期间
//     不能重新配置）。
//   - Python 用户代码内调用 input() 等阻塞式 stdin 操作会挂起该次调用直至
//     ExecTimeout 熔断（stdin 被协议独占）。
//   - 单会话同一时刻只能处理一次调用（execMu 串行化），高并发同 SessionID
//     调用会排队等待，不会并行执行、也不会互相污染状态。
type PersistentSandboxConfig struct {
	// IdleTTL 会话空闲超过此时长会被后台回收（默认 10 分钟）。
	IdleTTL time.Duration
	// MaxSessions 进程内并发存活会话上限，超限时淘汰最久未使用的会话（默认 8）。
	MaxSessions int
	// ExecTimeout 单次调用最长等待时间；超时视为该会话协议已不可信，整会话终止
	// （默认 30 秒）。必须显著小于 IdleTTL，否则空闲回收器可能在一次正常执行的
	// 中途误判为空闲——两者不是硬校验关系，仅在构造时用默认值保证这一点。
	ExecTimeout time.Duration
	// ReapInterval 空闲回收扫描周期（默认 30 秒）。
	ReapInterval time.Duration
}

func (c *PersistentSandboxConfig) applyDefaults() {
	if c.IdleTTL <= 0 {
		c.IdleTTL = 10 * time.Minute
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 8
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = 30 * time.Second
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = 30 * time.Second
	}
}

type PersistentSandbox struct {
	argvWrapper ArgvWrapper
	cfg         PersistentSandboxConfig

	pythonPath string
	bashPath   string

	mu       sync.Mutex
	sessions map[string]*liveSession

	stopReaper context.CancelFunc
}

// NewPersistentSandbox 构造 D4 长驻会话池。wrapper 为 nil 时 Available() 恒
// 为 false（未装配沙箱封装能力，fail-closed，不会退化为裸进程执行）。
// 后台空闲回收 goroutine 在构造时启动，随 Shutdown() 停止。
func NewPersistentSandbox(wrapper ArgvWrapper, cfg PersistentSandboxConfig) *PersistentSandbox {
	cfg.applyDefaults()
	p := &PersistentSandbox{
		argvWrapper: wrapper,
		cfg:         cfg,
		sessions:    make(map[string]*liveSession),
	}
	p.pythonPath, _ = exec.LookPath("python3")
	if p.pythonPath == "" {
		p.pythonPath, _ = exec.LookPath("python")
	}
	p.bashPath, _ = exec.LookPath("bash")

	reapCtx, cancel := context.WithCancel(context.Background())
	p.stopReaper = cancel
	concurrent.SafeGo(reapCtx, "sandbox_persistent.idle_reaper", p.reapLoop)
	return p
}

// Available 报告 L4 是否具备真实可用的能力：沙箱封装器已注入，且至少一种
// 受支持的解释器（python3/bash）在宿主 PATH 中可解析。不做"更深"的探测
// （比如真的 spawn 一个进程验证）——那属于 Run() 时才承担的成本，Available()
// 只做低成本的静态前置检查，供 RouteByTier 决定要不要走这条路由。
func (p *PersistentSandbox) Available() bool {
	if p.argvWrapper == nil {
		return false
	}
	return p.pythonPath != "" || p.bashPath != ""
}

// Backend 报告实际生效的后端标识（诊断/日志用途）。
func (p *PersistentSandbox) Backend() string {
	return "live_process_pool"
}

// Run 实现 SandboxProvider：执行一段代码，若 SessionID 对应的会话已存在且
// 语言匹配则复用，否则创建新会话。
func (p *PersistentSandbox) Run(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	if !p.Available() {
		return nil, apperr.New(apperr.CodeUnimplemented, "sandbox: L4 persistent backend unavailable on this host")
	}
	if spec.SessionID == "" {
		return nil, apperr.New(apperr.CodeInvalidInput, "sandbox: L4 persistent requires SessionID")
	}
	lang := spec.Language
	if lang != "python" && lang != "bash" {
		return nil, apperr.New(apperr.CodeInvalidInput, "sandbox: L4 persistent supports python/bash only, got "+lang)
	}
	if (lang == "python" && p.pythonPath == "") || (lang == "bash" && p.bashPath == "") {
		return nil, apperr.New(apperr.CodeUnimplemented, "sandbox: L4 persistent: interpreter for "+lang+" not found on host")
	}

	code, err := readSpecCode(spec)
	if err != nil {
		return nil, err
	}

	sess, err := p.getOrCreateSession(ctx, spec)
	if err != nil {
		return nil, err
	}

	execCtx, cancel := context.WithTimeout(ctx, p.cfg.ExecTimeout)
	defer cancel()

	result, execErr := sess.exec(execCtx, code)
	if execErr != nil {
		p.removeSession(spec.SessionID)
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent session execution failed", execErr)
	}
	return &types.ToolResult{
		Success:    result.ok,
		Output:     []byte(result.output),
		Error:      result.errorText,
		TaintLevel: spec.TaintLevel,
	}, nil
}

// readSpecCode 从 SandboxSpec 取出待执行代码文本：优先 ScriptBytes（测试/直接
// 下发场景），否则读取 ScriptPath（CodeAct 生产路径，见 code_act.go stageScript）。
func readSpecCode(spec SandboxSpec) (string, error) {
	if len(spec.ScriptBytes) > 0 {
		return string(spec.ScriptBytes), nil
	}
	if spec.ScriptPath == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "sandbox: L4 persistent requires ScriptPath or ScriptBytes")
	}
	data, err := os.ReadFile(spec.ScriptPath) //nolint:gosec // ScriptPath 由调用方（code_act stageScript）生成，非用户直接输入
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent: read script file", err)
	}
	return string(data), nil
}

func (p *PersistentSandbox) getOrCreateSession(ctx context.Context, spec SandboxSpec) (*liveSession, error) {
	p.mu.Lock()
	if sess, ok := p.sessions[spec.SessionID]; ok {
		if sess.language == spec.Language && sess.alive() {
			p.mu.Unlock()
			sess.touch()
			return sess, nil
		}
		// 语言不匹配或旧会话已失效：清理后重建。
		delete(p.sessions, spec.SessionID)
		p.mu.Unlock()
		sess.kill()
		p.mu.Lock()
	}
	if len(p.sessions) >= p.cfg.MaxSessions {
		p.evictOldestLocked()
	}
	p.mu.Unlock()

	sess, err := p.spawnSession(ctx, spec)
	if err != nil {
		return nil, err
	}
	sess.touch()

	p.mu.Lock()
	p.sessions[spec.SessionID] = sess
	p.mu.Unlock()
	return sess, nil
}

// evictOldestLocked 淘汰最久未使用的会话，调用方必须已持有 p.mu。
func (p *PersistentSandbox) evictOldestLocked() {
	var oldestID string
	var oldest *liveSession
	for id, sess := range p.sessions {
		if oldest == nil || sess.lastUsedNano.Load() < oldest.lastUsedNano.Load() {
			oldest, oldestID = sess, id
		}
	}
	if oldest != nil {
		delete(p.sessions, oldestID)
		oldest.kill()
	}
}

func (p *PersistentSandbox) removeSession(sessionID string) {
	p.mu.Lock()
	sess, ok := p.sessions[sessionID]
	if ok {
		delete(p.sessions, sessionID)
	}
	p.mu.Unlock()
	if ok {
		sess.kill()
	}
}

// spawnSession 通过 ArgvWrapper 取得沙箱封装后的 argv/env，构建并启动长驻
// 解释器进程，返回持有 stdin/stdout 管道的 liveSession。
func (p *PersistentSandbox) spawnSession(ctx context.Context, spec SandboxSpec) (*liveSession, error) {
	var execPath string
	var execArgs []string
	switch spec.Language {
	case "python":
		execPath = p.pythonPath
		execArgs = []string{"-u", "-c", pythonSessionHarness}
	case "bash":
		execPath = p.bashPath
		execArgs = []string{"--noprofile", "--norc", "-s"}
	}

	workDir, err := os.MkdirTemp("", "polaris_l4_session_")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent: create session workdir", err)
	}

	sctx := protocol.SandboxContext{
		CallerType:    protocol.CallerCodeAct,
		ExecPath:      execPath,
		ExecArgs:      execArgs,
		Workdir:       workDir,
		AllowedPaths:  append([]string{workDir}, spec.AllowedPaths...),
		NetworkPolicy: protocol.NetPolicyDeny,
		EnvExtra:      spec.ExtraEnv,
	}
	wrapped, err := p.argvWrapper.WrapArgv(ctx, sctx)
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent: sandbox wrap failed, refusing to spawn unsandboxed (fail-closed)", err)
	}

	cmd := exec.Command(wrapped.Executable, wrapped.Argv...) //nolint:gosec // Executable/Argv 来自 Rust 沙箱封装，非用户直接输入
	cmd.Dir = workDir
	if wrapped.EnvInArgv {
		cmd.Env = []string{}
	} else {
		cmd.Env = wrapped.Env
	}
	// DEBUG
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "PYTHON") || strings.HasPrefix(e, "CONDA") || strings.HasPrefix(e, "PATH") {
			fmt.Printf("DEBUG ENV: %s\n", e)
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent: create stdin pipe", err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent: create stdout pipe", err)
	}
	// stdout/stderr 合并到同一管道：长驻会话没有逐次独立的"这次调用的 stderr"
	// 边界，协议层（execPython 的 JSON 行 / execBash 的哨兵行）本身就承担了
	// 输出定界职责，与一次性沙箱按调用分别返回 stdout/stderr 的语义不同，
	// 已在类型级注释中记录为已知边界。
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		_ = os.RemoveAll(workDir)
		return nil, apperr.Wrap(apperr.CodeInternal, "sandbox: L4 persistent: spawn failed", err)
	}
	_ = pw.Close() // 父进程侧写端副本关闭；子进程持有自己的副本，管道不会提前 EOF

	sess := &liveSession{
		sessionID: spec.SessionID,
		language:  spec.Language,
		workDir:   workDir,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    bufio.NewReader(pr),
		pr:        pr,
		sentinel:  uuid.NewString(),
		createdAt: time.Now(),
	}
	return sess, nil
}

// Shutdown 终止全部存活会话并停止空闲回收 goroutine。供 cmd/polaris 优雅关闭
// 时调用；nil-safe（*PersistentSandbox 为 nil 时不 panic，见方法内判空）。
func (p *PersistentSandbox) Shutdown() {
	if p == nil {
		return
	}
	if p.stopReaper != nil {
		p.stopReaper()
	}
	p.mu.Lock()
	sessions := p.sessions
	p.sessions = make(map[string]*liveSession)
	p.mu.Unlock()
	for _, sess := range sessions {
		sess.kill()
	}
}

func (p *PersistentSandbox) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reapIdleSessions()
		}
	}
}

func (p *PersistentSandbox) reapIdleSessions() {
	p.mu.Lock()
	var toReap []*liveSession
	for id, sess := range p.sessions {
		if !sess.alive() || sess.idleSince() > p.cfg.IdleTTL {
			toReap = append(toReap, sess)
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	for _, sess := range toReap {
		sess.kill()
	}
}
