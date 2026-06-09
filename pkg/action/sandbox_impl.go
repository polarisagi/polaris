package action

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// SandboxProvider 是沙箱执行抽象接口，允许对 Wasm/InProcess/Container 分别实现。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2
type SandboxProvider interface {
	// Run 执行工具并返回结果。spec 描述执行约束。
	Run(ctx context.Context, spec SandboxSpec) (*protocol.ToolResult, error)
}

// SandboxSpec 描述一次沙箱执行的完整规格。
type SandboxSpec struct {
	ToolName     string
	Input        []byte
	SandboxTier  protocol.SandboxTier
	Capability   protocol.CapabilityLevel
	SideEffects  []protocol.SideEffect
	WasmPath     string   // SandboxTier=Wasm 时必填
	WasmBytes    []byte   // 直接传入的 Wasm 字节码 (主要用于测试或直接下发)
	AllowedPaths []string // 文件系统白名单
	CPUQuotaMs   int      // 0 = 默认 5000ms
	IOBudget     int64    // 0 = 默认 8MB
	MaxCalls     int      // 0 = 默认 10000
}

// ─── Tier 1: InProcessSandbox ────────────────────────────────────────────────

// InProcessSandbox 在调用方 goroutine 内直接执行内置工具函数。
// 适用于: protocol.ToolBuiltin + protocol.CapReadOnly
// 安全约束: 无文件写、无网络——由 PolicyGate 在调用前验证，此处不再重复校验。
type InProcessSandbox struct {
	mu       sync.RWMutex
	registry map[string]InProcessFn
	// richRegistry 存储可返回 ToolResult（含 ImageParts）的富工具函数（MCP 等外部工具）。
	// Run() 优先查此表，未命中才走 registry，两表互斥（RegisterRich 不写 registry）。
	richRegistry map[string]InProcessRichFn
	// taintMap 存储每个工具的输出污点等级。
	// 内置工具保持 TaintNone（零值），MCP/外部工具通过 RegisterWithTaint/RegisterRich 写入。
	taintMap map[string]protocol.TaintLevel
}

// InProcessFn 内置工具执行函数签名（仅返回字节）。
type InProcessFn func(ctx context.Context, input []byte) ([]byte, error)

// InProcessRichFn 富工具执行函数签名，返回完整 ToolResult（含 ImageParts）。
// 适用于 MCP 工具等可能返回图片/多媒体内容的外部工具。
// 调用方（InProcessSandbox.Run）会将 ToolResult.TaintLevel 设为注册时指定的 taint（若未设置）。
type InProcessRichFn func(ctx context.Context, input []byte) (*protocol.ToolResult, error)

func NewInProcessSandbox() *InProcessSandbox {
	return &InProcessSandbox{
		registry:     make(map[string]InProcessFn),
		richRegistry: make(map[string]InProcessRichFn),
		taintMap:     make(map[string]protocol.TaintLevel),
	}
}

// Register 注册工具函数（并发安全，支持运行时动态注册 MCP 工具）。
// 内置工具使用此方法，输出污点为 TaintNone。
func (s *InProcessSandbox) Register(toolName string, fn InProcessFn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[toolName] = fn
}

// RegisterWithTaint 注册工具函数并指定输出污点等级。
// MCP/外部工具调用此方法：白名单 → TaintMedium，其余 → TaintHigh。
func (s *InProcessSandbox) RegisterWithTaint(toolName string, fn InProcessFn, taint protocol.TaintLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[toolName] = fn
	s.taintMap[toolName] = taint
}

// RegisterRich 注册富工具函数（返回完整 ToolResult，含 ImageParts）。
// 供 MCP/外部工具使用；taint 在 Run() 中回填（若 ToolResult.TaintLevel==0）。
// 不同于 Register/RegisterWithTaint：不写 registry，两路互斥。
func (s *InProcessSandbox) RegisterRich(toolName string, fn InProcessRichFn, taint protocol.TaintLevel) {
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

func (s *InProcessSandbox) Run(ctx context.Context, spec SandboxSpec) (*protocol.ToolResult, error) {
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

	start := time.Now()

	// 优先走富工具路径（MCP 等可返回 ImageParts 的工具）
	if richFn != nil {
		res, err := richFn(execCtx, spec.Input)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			return &protocol.ToolResult{
				Success:    false,
				Error:      err.Error(),
				LatencyMs:  latency,
				TaintLevel: taint,
			}, nil
		}
		if res == nil {
			res = &protocol.ToolResult{}
		}
		res.LatencyMs = latency
		// 回填注册时的污点等级（富工具函数通常不感知 taint，由注册层统一设置）
		if res.TaintLevel == 0 {
			res.TaintLevel = taint
		}
		return res, nil
	}

	if !ok {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("inprocess_sandbox: unknown tool %q", spec.ToolName))
	}

	out, err := fn(execCtx, spec.Input)
	if err != nil {
		return &protocol.ToolResult{
			Success:    false,
			Error:      err.Error(),
			LatencyMs:  time.Since(start).Milliseconds(),
			TaintLevel: taint,
		}, nil
	}
	return &protocol.ToolResult{
		Success:    true,
		Output:     out,
		LatencyMs:  time.Since(start).Milliseconds(),
		TaintLevel: taint,
	}, nil
}

// Execute 满足 tool.SandboxExecutor 接口（简化版，无 SandboxSpec 包装），
// 允许 InProcessSandbox 直接作为 InMemoryToolRegistry 的执行后端。
func (s *InProcessSandbox) Execute(ctx context.Context, toolName string, input []byte) ([]byte, error) {
	s.mu.RLock()
	fn, ok := s.registry[toolName]
	s.mu.RUnlock()
	if !ok {
		return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("inprocess_sandbox: unknown tool %q", toolName))
	}
	return fn(ctx, input)
}

// ─── Tier 2: WasmSandbox ─────────────────────────────────────────────────────

// WasmSandbox 通过 wazero 执行 Wasm 二进制。
// MVP: wazero 实体由 Rust 编译生成，目前以 stub 验证集成路径。
// 适用于: protocol.ToolMCP / ToolA2A / ToolLLMGenerated
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.3
type WasmSandbox struct {
	runtime *WazeroRuntime
}

func NewWasmSandbox(ctx context.Context, concurrency int) *WasmSandbox {
	return &WasmSandbox{
		runtime: NewWazeroRuntime(ctx, concurrency),
	}
}

func (s *WasmSandbox) PreWarmCache(skillID string, wasmBytes []byte) error {
	return s.runtime.PreWarmCache(skillID, wasmBytes)
}

func (s *WasmSandbox) Run(ctx context.Context, spec SandboxSpec) (*protocol.ToolResult, error) {
	quotaMs := spec.CPUQuotaMs
	if quotaMs == 0 {
		quotaMs = 5000
	}
	ioBudget := spec.IOBudget
	if ioBudget == 0 {
		ioBudget = 8 * 1024 * 1024 // 8MB
	}
	maxCalls := spec.MaxCalls
	if maxCalls == 0 {
		maxCalls = 10000
	}

	config := &ExecuteConfig{
		Capability:     int(spec.Capability),
		SandboxTier:    int(spec.SandboxTier),
		CPUQuotaMs:     quotaMs,
		WallClockLimit: quotaMs * 3,
		IOBudgetBytes:  ioBudget,
		MaxHostCall:    maxCalls,
		AllowedPaths:   spec.AllowedPaths,
		WasmBytes:      spec.WasmBytes,
	}

	return s.runtime.RunWasm(ctx, spec.ToolName, spec.Input, config, protocol.TaintNone)
}

// ─── Tier 3: ContainerSandbox ────────────────────────────────────────────────

// ContainerSandbox 通过 OS 子进程（未来集成 gVisor/Docker）执行特权工具。
// 当前 MVP 实现：通过 exec.Command 在限制环境中执行二进制。
// 适用于: protocol.CapPrivileged + protocol.SideProcessSpawn
// 约束: Tier 0 (8GB Linux) + 非 Linux 回退至 WasmSandbox
type ContainerSandbox struct {
	binPath  string // 沙箱执行器二进制路径（如 /usr/local/bin/polaris-sandbox）
	platform string
	hwTier   observability.Tier
}

func NewContainerSandbox(binPath, platform string, hwTier observability.Tier) *ContainerSandbox {
	return &ContainerSandbox{binPath: binPath, platform: platform, hwTier: hwTier}
}

func (s *ContainerSandbox) Run(ctx context.Context, spec SandboxSpec) (*protocol.ToolResult, error) {
	quotaMs := spec.CPUQuotaMs
	if quotaMs == 0 {
		quotaMs = 30000
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(quotaMs)*time.Millisecond)
	defer cancel()

	l3Available, backend := observability.TierSandboxConfig(s.hwTier, s.platform)
	if !l3Available {
		return &protocol.ToolResult{Success: false, Error: "ContainerSandbox: L3 not available on this tier/platform"}, nil
	}

	var cmd *exec.Cmd
	switch backend {
	case "firecracker":
		cmd = buildFirecrackerCmd(execCtx, s.binPath, spec)
	case "virtualization_framework":
		cmd = buildVZCmd(execCtx, s.binPath, spec)
	case "wsl2":
		cmd = buildWSL2Cmd(execCtx, s.binPath, spec)
	default:
		return &protocol.ToolResult{Success: false, Error: fmt.Sprintf("ContainerSandbox: unknown backend %q", backend)}, nil
	}

	start := time.Now()
	out, err := cmd.Output()
	if err != nil {
		return &protocol.ToolResult{
			Success:   false,
			Error:     err.Error(),
			LatencyMs: time.Since(start).Milliseconds(),
		}, nil
	}
	return &protocol.ToolResult{
		Success:   true,
		Output:    out,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// RunScript 在与 ContainerSandbox 相同的 OS 命名空间隔离下直接执行任意脚本。
// 适用于插件 uninstall hook 等需要运行任意二进制路径的场景。
// workDir 为脚本工作目录；超时固定 30s（与 Run 一致）。
func (s *ContainerSandbox) RunScript(ctx context.Context, scriptPath, workDir string) error {
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, scriptPath)
	cmd.Dir = workDir
	// Linux: 注入命名空间隔离（与 Run 共用，防止 hook 逃逸宿主 PID/NS 空间）
	if attrs := containerSandboxSysProcAttr(); attrs != nil {
		cmd.SysProcAttr = attrs
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sandbox: RunScript %q: %w", scriptPath, err)
	}
	return nil
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

// ─── SandboxRouter ────────────────────────────────────────────────────────────

// SandboxRouter 根据 SandboxSpec.SandboxTier 路由至对应沙箱实现。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2 三层矩阵
type SandboxRouter struct {
	inProcess *InProcessSandbox
	wasm      *WasmSandbox
	container *ContainerSandbox
	remote    *RemoteSandbox // L4：可选，Tier-0 OOM 逃生路径
	goos      string         // "darwin" | "linux" | "windows"
	hwTier    int            // 0 = Tier 0 (8GB) 主线
}

func NewSandboxRouter(inProcess *InProcessSandbox, wasm *WasmSandbox, container *ContainerSandbox, goos string, hwTier int) *SandboxRouter {
	return &SandboxRouter{
		inProcess: inProcess,
		wasm:      wasm,
		container: container,
		goos:      goos,
		hwTier:    hwTier,
	}
}

// WithRemote 注入 Remote Sandbox（可选）。返回自身，支持链式调用。
// 配置后，SandboxRemote 层级工具和 Tier-0 非 Linux 下 SandboxContainer 的降级均路由至此。
func (r *SandboxRouter) WithRemote(remote *RemoteSandbox) *SandboxRouter {
	r.remote = remote
	return r
}

// Route 根据工具属性选择最合适的沙箱，返回 SandboxProvider。
// 规则与 AssignSandboxTier 保持一致。
func (r *SandboxRouter) Route(tool protocol.Tool) SandboxProvider {
	tier := AssignSandboxTier(tool, r.hwTier, r.goos)
	switch tier {
	case protocol.SandboxRemote:
		if r.remote != nil {
			return r.remote
		}
		// 未配置远端，降级到 container/wasm/inProcess 链
		fallthrough
	case protocol.SandboxContainer:
		if r.container != nil {
			return r.container
		}
		// Tier-0 非 Linux 无 L3：优先走 Remote（逃生路径），再降 L2
		if r.remote != nil {
			return r.remote
		}
		if r.wasm != nil {
			return r.wasm
		}
		return r.inProcess
	case protocol.SandboxWasm:
		if r.wasm != nil {
			return r.wasm
		}
		// L2Sandbox 被 FeatureGate 禁用时降级到 in-process
		return r.inProcess
	default: // SandboxInProcess
		return r.inProcess
	}
}

// Execute 完整执行路径：Route → Run → ToolResult。
// SandboxSpec.SandboxTier 使用 AssignSandboxTier 升级后的实际 tier，保证审计信息与执行一致。
func (r *SandboxRouter) Execute(ctx context.Context, tool protocol.Tool, input []byte) (*protocol.ToolResult, error) {
	actualTier := AssignSandboxTier(tool, r.hwTier, r.goos)
	provider := r.Route(tool)
	spec := SandboxSpec{
		ToolName:    tool.Name,
		Input:       input,
		SandboxTier: actualTier,
		Capability:  tool.Capability,
		SideEffects: tool.SideEffects,
		CPUQuotaMs:  int(tool.Timeout.Milliseconds()),
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
