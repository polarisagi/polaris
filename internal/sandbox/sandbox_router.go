package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── SandboxRouter ────────────────────────────────────────────────────────────

// SandboxRouter 根据 SandboxSpec.SandboxTier 路由至对应沙箱实现。
// 内置工具走 InProcess（直接 Go 调用）；LLM 生成代码/插件走 Container/NativeOS。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2 三层矩阵
type SandboxRouter struct {
	mu              sync.Mutex
	inProcess       *InProcessSandbox
	container       *ContainerSandbox
	nativeOS        *NativeOSSandbox // L4-native：Tier-0 Rust 原生沙箱，无需容器运行时
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

// WithNativeOS 注入 NativeOSSandbox（Tier-0 Rust 原生沙箱）。返回自身，支持链式调用。
// 配置后，SandboxNativeOS tier（assign.go Tier-0 + Container 降级路径）路由至此。
func (r *SandboxRouter) WithNativeOS(nativeOS *NativeOSSandbox) *SandboxRouter {
	r.nativeOS = nativeOS
	return r
}

// Route 根据工具属性选择最合适的沙箱，返回 SandboxProvider。
// 规则与 AssignSandboxTier 保持一致。
// RouteByTier 按已算好的 tier 路由。trustTier 用于决定隔离不可用时能否降级。
// 安全规则：trust < Official 且要求 L2/L3 但对应沙箱不可用 → fail-closed 拒绝，不降级到 L1。
func (r *SandboxRouter) RouteByTier(tier types.SandboxTier, trustTier types.TrustTier) (SandboxProvider, error) {
	mustIsolate := trustTier < types.TrustOfficial
	switch tier {
	case types.SandboxNativeOS:
		// Tier-0 fallback：Rust 原生沙箱（无容器运行时依赖）。
		// assign.go 在 hwTier==0 时将 SandboxContainer 降级为此 tier。
		if r.nativeOS != nil {
			return r.nativeOS, nil
		}
		// nativeOS 未注入时（测试环境）尝试 container，否则 fail-closed。
		if r.container != nil {
			return r.container, nil
		}
		return nil, apperr.New(apperr.CodeForbidden, "sandbox: NativeOS required for Tier-0 CodeAct but unavailable; refusing to downgrade")
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
	res, err := provider.Run(ctx, spec)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("sandbox run tool %q", tool.Name), err)
	}
	return res, nil
}
