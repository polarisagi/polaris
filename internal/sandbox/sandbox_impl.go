package sandbox

import (
	"github.com/polarisagi/polaris/internal/observability/trace"

	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
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
	AllowedPaths []string // 文件系统白名单
	CPUQuotaMs   int      // 0 = 默认 5000ms
	IOBudget     int64    // 0 = 默认 8MB
	MaxCalls     int      // 0 = 默认 10000
	SystemTier   int      // 硬件分级
	TaintLevel   types.TaintLevel
	DryRunMode   bool
	MockProxyEnv string
}

// ─── Tier 1: InProcessSandbox ────────────────────────────────────────────────

// InProcessSandbox 在调用方 goroutine 内直接执行内置工具函数。
// 适用于: types.ToolBuiltin + types.CapReadOnly
// 安全约束: 无文件写、无网络——由 PolicyGate 在调用前验证，此处不再重复校验。
type InProcessSandbox struct {
	mu       sync.RWMutex
	registry map[string]InProcessFn
	// richRegistry 存储可返回 ToolResult（含 ImageParts）的富工具函数（MCP 等外部工具）。
	// Run() 优先查此表，未命中才走 registry，两表互斥（RegisterRich 不写 registry）。
	richRegistry map[string]InProcessRichFn
	// taintMap 存储每个工具的输出污点等级。
	// 内置工具保持 TaintNone（零值），MCP/外部工具通过 RegisterWithTaint/RegisterRich 写入。
	taintMap map[string]types.TaintLevel
}

// InProcessFn 内置工具执行函数签名（仅返回字节）。
type InProcessFn func(ctx context.Context, input []byte) ([]byte, error)

// InProcessRichFn 富工具执行函数签名，返回完整 ToolResult（含 ImageParts）。
// 适用于 MCP 工具等可能返回图片/多媒体内容的外部工具。
// 调用方（InProcessSandbox.Run）会将 ToolResult.TaintLevel 设为注册时指定的 taint（若未设置）。
type InProcessRichFn func(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error)

func NewInProcessSandbox() *InProcessSandbox {
	return &InProcessSandbox{
		registry:     make(map[string]InProcessFn),
		richRegistry: make(map[string]InProcessRichFn),
		taintMap:     make(map[string]types.TaintLevel),
	}
}

// Level 返回沙箱级别（实现 protocol.SandboxProvider）。
func (s *InProcessSandbox) Level() int { return 1 }

// Register 注册工具函数（并发安全，支持运行时动态注册 MCP 工具）。
// 内置工具使用此方法，输出污点为 TaintNone。
func (s *InProcessSandbox) Register(toolName string, fn InProcessFn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[toolName] = fn
}

// RegisterWithTaint 注册工具函数并指定输出污点等级。
// MCP/外部工具调用此方法：白名单 → TaintMedium，其余 → TaintHigh。
func (s *InProcessSandbox) RegisterWithTaint(toolName string, fn InProcessFn, taint types.TaintLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[toolName] = fn
	s.taintMap[toolName] = taint
}

// RegisterRich 注册富工具函数（返回完整 ToolResult，含 ImageParts）。
// 供 MCP/外部工具使用；taint 在 Run() 中回填（若 ToolResult.TaintLevel==0）。
// 不同于 Register/RegisterWithTaint：不写 registry，两路互斥。
func (s *InProcessSandbox) RegisterRich(toolName string, fn InProcessRichFn, taint types.TaintLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.richRegistry[toolName] = fn
	s.taintMap[toolName] = taint
}

// Unregister 取消注册工具（MCP Server 断开时调用，同时清理两个注册表）。
func (s *InProcessSandbox) Unregister(toolName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.registry, toolName)
	delete(s.richRegistry, toolName)
	delete(s.taintMap, toolName)
}

func (s *InProcessSandbox) Run(ctx context.Context, spec SandboxSpec) (result *types.ToolResult, runErr error) {
	start := time.Now()
	tierLabel := trace.SandboxTierLabel(int(spec.SandboxTier))
	defer func() {
		latencyMs := float64(time.Since(start).Milliseconds())
		status := "success"
		if runErr != nil {
			status = "error"
		}
		trace.RecordToolCall(ctx, spec.ToolName, status, tierLabel, latencyMs)
		trace.RecordSandboxExecution(ctx, tierLabel)
	}()

	s.mu.RLock()
	fn, ok := s.registry[spec.ToolName]
	richFn := s.richRegistry[spec.ToolName]
	taint := s.taintMap[spec.ToolName] // TaintNone(0) for builtins
	s.mu.RUnlock()

	quotaMs := spec.CPUQuotaMs
	if quotaMs == 0 {
		quotaMs = 5000
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(quotaMs)*time.Millisecond)
	defer cancel()

	// 优先走富工具路径（MCP 等可返回 ImageParts 的工具）
	if richFn != nil {
		// InProcessRichFn 工具执行
		res, execErr := richFn(execCtx, spec)
		latency := time.Since(start).Milliseconds()
		if execErr != nil {
			return &types.ToolResult{
				Success:    false,
				Error:      execErr.Error(),
				LatencyMs:  latency,
				TaintLevel: taint,
			}, nil
		}
		if res == nil {
			res = &types.ToolResult{}
		}
		res.LatencyMs = latency
		// 回填注册时的污点等级（富工具函数通常不感知 taint，由注册层统一设置）
		if res.TaintLevel == 0 {
			res.TaintLevel = taint
		}
		return res, nil
	}

	if !ok {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("inprocess_sandbox: unknown tool %q", spec.ToolName))
	}

	out, execErr := fn(execCtx, spec.Input)
	if execErr != nil {
		return &types.ToolResult{
			Success:    false,
			Error:      execErr.Error(),
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taint,
		}, nil
	}
	return &types.ToolResult{
		Success:    true,
		Output:     out,
		LatencyMs:  time.Since(start).Milliseconds(),
		TaintLevel: taint,
	}, nil
}

// Execute 满足 tool.SandboxExecutor 接口（简化版，无 SandboxSpec 包装），
// 允许 InProcessSandbox 直接作为 InMemoryToolRegistry 的执行后端。
func (s *InProcessSandbox) Execute(ctx context.Context, toolName string, input []byte, taintLevel types.TaintLevel) ([]byte, error) {
	s.mu.RLock()
	fn, ok := s.registry[toolName]
	s.mu.RUnlock()
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("inprocess_sandbox: unknown tool %q", toolName))
	}
	return fn(ctx, input)
}

// ─── Tier 3: ContainerSandbox ────────────────────────────────────────────────

// ContainerSandbox 通过 OS 子进程（未来集成 gVisor/Docker）执行特权工具。
// 当前 MVP 实现：通过 exec.Command 在限制环境中执行二进制。
// 适用于: types.CapPrivileged / TypeScript 脚本技能 / LLM 生成代码执行
type ContainerSandbox struct {
	binPath  string // 沙箱执行器二进制路径（如 /usr/local/bin/polaris-sandbox）
	platform string
	hwTier   probe.Tier
}

func NewContainerSandbox(binPath, platform string, hwTier probe.Tier) *ContainerSandbox {
	return &ContainerSandbox{binPath: binPath, platform: platform, hwTier: hwTier}
}

// Level 返回沙箱级别（实现 protocol.SandboxProvider）。
func (s *ContainerSandbox) Level() int { return 3 }

func (s *ContainerSandbox) Run(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	quotaMs := spec.CPUQuotaMs
	if quotaMs == 0 {
		quotaMs = 30000
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(quotaMs)*time.Millisecond)
	defer cancel()

	// CodeAct / 技能脚本直接执行路径：无需 Firecracker/VZ 基础设施，
	// 由 runNativeScript 通过 OS 命名空间隔离直接执行脚本文件。
	if spec.ScriptPath != "" {
		return s.runNativeScript(execCtx, spec)
	}

	l3Available, backend := probe.TierSandboxConfig(s.hwTier, s.platform)
	if !l3Available {
		return &types.ToolResult{Success: false, Error: "ContainerSandbox: L3 not available on this tier/platform"}, nil
	}

	var cmd *exec.Cmd
	switch backend {
	case "firecracker":
		cmd = buildFirecrackerCmd(execCtx, s.binPath, spec)
	case "virtualization_framework":
		cmd = buildVZCmd(execCtx, s.binPath, spec)
	case "wsl2":
		cmd = buildWSL2Cmd(execCtx, s.binPath, spec)
	case "native":
		// Tier1 Linux：bwrap + 命名空间隔离（无 Firecracker 基础设施的降级路径）
		cmd = buildNativeCmd(execCtx, s.binPath, spec)
	default:
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("ContainerSandbox: unknown backend %q", backend)}, nil
	}

	// 始终消毒环境变量，防止父进程凭据泄漏（R1.15）。
	// DryRunMode 在最小环境基础上再叠加 mock proxy 变量；生产路径用相同最小环境。
	env := sandboxMinEnv()
	if spec.DryRunMode {
		mockTable := make(map[string]MockResponse)
		proxy, proxyAddr, errProxy := NewMockProxy(mockTable)
		if errProxy != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "create mock proxy failed", errProxy)
		}
		defer proxy.Close()
		for k, v := range proxy.EnvVars() {
			env = append(env, k+"="+v)
		}
		_ = proxyAddr
	}
	cmd.Env = env

	start := time.Now()
	out, err := cmd.Output()
	if err != nil {
		return &types.ToolResult{
			Success:   false,
			Error:     err.Error(),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}
	return &types.ToolResult{
		Success:   true,
		Output:    out,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// RunHook 在与 ContainerSandbox 相同的 OS 命名空间隔离下直接执行任意脚本。
// 适用于插件 uninstall hook 等需要运行任意二进制路径的场景。
// workDir 为脚本工作目录；超时固定 30s（与 Run 一致）。
func (s *ContainerSandbox) RunHook(ctx context.Context, scriptPath, workDir string) error {
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, scriptPath)
	cmd.Dir = workDir
	cmd.Env = sandboxMinEnv() // 防止父进程凭据泄漏（R1.15）
	// Linux: 注入命名空间隔离（与 Run 共用，防止 hook 逃逸宿主 PID/NS 空间）
	if attrs := containerSandboxSysProcAttr(); attrs != nil {
		cmd.SysProcAttr = attrs
	}
	if err := cmd.Run(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("sandbox: RunHook %q", scriptPath), err)
	}
	return nil
}

func (s *ContainerSandbox) RunScript(ctx context.Context, skillName, scriptPath string, input []byte, trustTier types.TrustTier) ([]byte, error) {
	tool := types.Tool{Name: skillName, Source: types.ToolLLMGenerated, TrustTier: trustTier}
	tier, err := AssignSandboxTier(tool, tool.TrustTier, int(s.hwTier), s.platform)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeSandboxTier0Limit, "skill: tier rejected", err)
	}
	res, err := s.Run(ctx, SandboxSpec{
		ToolName: skillName, Input: input, SandboxTier: tier,
		ScriptPath: scriptPath, CPUQuotaMs: 30000,
	})
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, apperr.New(apperr.CodeInternal, "skill: script failed: "+res.Error)
	}
	return res.Output, nil
}

func buildFirecrackerCmd(ctx context.Context, jailerPath string, spec SandboxSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, jailerPath, "--id", spec.ToolName, "--exec-file", "/usr/local/bin/firecracker")
	cmd.Stdin = bytes2ReadCloser(spec.Input)
	if attrs := containerSandboxSysProcAttr(); attrs != nil {
		cmd.SysProcAttr = attrs
	}
	return cmd
}

func buildVZCmd(ctx context.Context, vftoolPath string, spec SandboxSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, vftoolPath, "--tool", spec.ToolName)
	cmd.Stdin = bytes2ReadCloser(spec.Input)
	return cmd
}

func buildWSL2Cmd(ctx context.Context, binPath string, spec SandboxSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "wsl.exe", "-e", binPath, "--tool", spec.ToolName)
	cmd.Stdin = bytes2ReadCloser(spec.Input)
	return cmd
}

// buildNativeCmd 使用 bwrap/命名空间隔离执行沙箱工具二进制（Tier1 Linux native 后端）。
// 复用 polaris-sandbox 二进制，通过 Linux 命名空间隔离代替 Firecracker 提供进程隔离。
func buildNativeCmd(ctx context.Context, binPath string, spec SandboxSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binPath, "--tool", spec.ToolName)
	cmd.Stdin = bytes2ReadCloser(spec.Input)
	if attrs := containerSandboxSysProcAttr(); attrs != nil {
		cmd.SysProcAttr = attrs
	}
	return cmd
}

// runNativeScript 在 OS 原生进程隔离中执行脚本文件（CodeAct + 技能脚本路径）。
// 解释器由脚本后缀推断：.py → python3, .sh/.bash → bash。
// Linux 通过 SysProcAttr 注入命名空间隔离，macOS 仅依赖 cmd.Env 消毒。
func (s *ContainerSandbox) runNativeScript(ctx context.Context, spec SandboxSpec) (*types.ToolResult, error) {
	interp, err := resolveInterpreter(spec.ScriptPath)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil //nolint:nilerr // 解释器解析失败作为工具级错误上报，不向调用方传播
	}

	cmd := exec.CommandContext(ctx, interp, spec.ScriptPath)
	// 使用生产环境基础变量（含语言运行时路径），不传入凭据（R1.15）
	cmd.Env = containerBaseEnv()
	if attrs := containerSandboxSysProcAttr(); attrs != nil {
		cmd.SysProcAttr = attrs
	}

	start := time.Now()
	out, runErr := cmd.Output()
	latency := time.Since(start).Milliseconds()
	if runErr != nil {
		// cmd.Output() 在非零退出码时返回 *exec.ExitError，Output 字段含 stderr
		exitOut := out
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && len(ee.Stderr) > 0 {
			exitOut = ee.Stderr
		}
		return &types.ToolResult{
			Success:   false,
			Error:     runErr.Error(),
			Output:    exitOut,
			LatencyMs: latency,
		}, nil
	}
	return &types.ToolResult{
		Success:   true,
		Output:    out,
		LatencyMs: latency,
	}, nil
}

// resolveInterpreter 从脚本后缀推断解释器绝对路径。
func resolveInterpreter(scriptPath string) (string, error) {
	switch strings.ToLower(filepath.Ext(scriptPath)) {
	case ".py":
		for _, name := range []string{"python3", "python"} {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
		}
		return "", apperr.New(apperr.CodeInternal, "sandbox: python3/python not found in PATH")
	case ".sh", ".bash":
		for _, name := range []string{"bash", "sh"} {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
		}
		return "", apperr.New(apperr.CodeInternal, "sandbox: bash/sh not found in PATH")
	default:
		return "", apperr.New(apperr.CodeInternal,
			fmt.Sprintf("sandbox: unsupported script extension %q", filepath.Ext(scriptPath)))
	}
}

// ─── SandboxRouter ────────────────────────────────────────────────────────────

// SandboxRouter 根据 SandboxSpec.SandboxTier 路由至对应沙箱实现。
// 内置工具走 InProcess（直接 Go 调用）；LLM 生成代码/插件走 Container。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2 三层矩阵
type SandboxRouter struct {
	mu              sync.Mutex
	inProcess       *InProcessSandbox
	container       *ContainerSandbox
	wasmtime        SandboxProvider
	remote          *RemoteSandbox // L4：可选，Tier-0 OOM 逃生路径
	goos            string         // "darwin" | "linux" | "windows"
	hwTier          int            // 0 = Tier 0 (8GB) 主线
	newWasmDisabled atomic.Bool
	activeWasm      map[string]context.CancelFunc
}

func NewSandboxRouter(inProcess *InProcessSandbox, container *ContainerSandbox, wasmtime SandboxProvider, goos string, hwTier int) *SandboxRouter {
	return &SandboxRouter{
		inProcess:  inProcess,
		container:  container,
		wasmtime:   wasmtime,
		goos:       goos,
		hwTier:     hwTier,
		activeWasm: make(map[string]context.CancelFunc),
	}
}

// DisableNewInstances 满足 observability.SandboxController，禁止启动新 Wasm 实例（L1 预警）。
func (r *SandboxRouter) DisableNewInstances(disable bool) {
	r.newWasmDisabled.Store(disable)
}

// KillIdleSandboxes 回收空闲的 WasmSandbox 实例（OSMemoryGuard L2 级调用）。
// 当前设计无长期驻留的空闲沙箱进程（InProcess/Wasm 均为请求粒度），清理计数器统计即可。
func (r *SandboxRouter) KillIdleSandboxes(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := int64(len(r.activeWasm))
	for k, cancel := range r.activeWasm {
		cancel()
		delete(r.activeWasm, k)
	}
	if count > 0 {
		slog.InfoContext(ctx, "sandbox: killed idle wasm instances", "count", count)
	}
}

// KillAllNonCritical 回收全部非关键沙箱（OSMemoryGuard L3 临界内存压力调用）。
// 强制终止所有已知的 WasmSandbox + ContainerSandbox 实例，优先级低于 InProcess。
func (r *SandboxRouter) KillAllNonCritical(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := int64(len(r.activeWasm))
	for k, cancel := range r.activeWasm {
		cancel()
		delete(r.activeWasm, k)
	}
	slog.WarnContext(ctx, "sandbox: killed all non-critical sandboxes (L3 memory pressure)", "count", count)
}

// WithRemote 注入 Remote Sandbox（可选）。返回自身，支持链式调用。
// 配置后，SandboxRemote 层级工具和 Tier-0 非 Linux 下 SandboxContainer 的降级均路由至此。
func (r *SandboxRouter) WithRemote(remote *RemoteSandbox) *SandboxRouter {
	r.remote = remote
	return r
}

// Route 根据工具属性选择最合适的沙箱，返回 SandboxProvider。
// 规则与 AssignSandboxTier 保持一致。
// RouteByTier 按已算好的 tier 路由。trustTier 用于决定隔离不可用时能否降级。
// 安全规则：trust < Official 且要求 L2/L3 但对应沙箱不可用 → fail-closed 拒绝，不降级到 L1。
func (r *SandboxRouter) RouteByTier(tier types.SandboxTier, trustTier types.TrustTier) (SandboxProvider, error) {
	mustIsolate := trustTier < types.TrustOfficial
	switch tier {
	case types.SandboxRemote:
		if r.remote != nil {
			return r.remote, nil
		}
		fallthrough
	case types.SandboxWasm:
		if r.wasmtime != nil {
			return r.wasmtime, nil
		}
		if r.container != nil {
			return r.container, nil
		}
		if r.remote != nil {
			return r.remote, nil
		}
		if mustIsolate {
			return nil, apperr.New(apperr.CodeForbidden, "sandbox: L2/Wasm required for untrusted code but unavailable; refusing to downgrade")
		}
		slog.Warn("sandbox: Wasm 不可用，可信来源降级 InProcess")
		return r.inProcess, nil
	case types.SandboxContainer:
		if r.container != nil {
			return r.container, nil
		}
		if r.remote != nil {
			return r.remote, nil
		}
		return nil, apperr.New(apperr.CodeForbidden, "sandbox: L3/Container required but unavailable; refusing to downgrade")
	default: // InProcess
		return r.inProcess, nil
	}
}

// Execute 完整执行路径：Route → Run → ToolResult。
// SandboxSpec.SandboxTier 使用 AssignSandboxTier 升级后的实际 tier，保证审计信息与执行一致。
func (r *SandboxRouter) Execute(ctx context.Context, tool types.Tool, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	tier, err := AssignSandboxTier(tool, tool.TrustTier, r.hwTier, r.goos)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeSandboxTier0Limit, "sandbox tier assignment rejected", err)
	}
	provider, err := r.RouteByTier(tier, tool.TrustTier)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("sandbox route tool %q", tool.Name), err)
	}
	spec := SandboxSpec{
		ToolName:    tool.Name,
		Input:       input,
		SandboxTier: tier,
		Capability:  tool.Capability,
		SideEffects: tool.SideEffects,
		CPUQuotaMs:  int(tool.Timeout.Milliseconds()),
		SystemTier:  r.hwTier,
		TaintLevel:  taintLevel,
	}
	return provider.Run(ctx, spec)
}

// ─── 工具函数 ─────────────────────────────────────────────────────────────────

// bytes2ReadCloser 将 []byte 封装为 io.ReadCloser（供 ContainerSandbox stdin 使用）。
func bytes2ReadCloser(b []byte) *noopReadCloser {
	return &noopReadCloser{data: b, pos: 0}
}

type noopReadCloser struct {
	data []byte
	pos  int
}

func (r *noopReadCloser) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *noopReadCloser) Close() error { return nil }
