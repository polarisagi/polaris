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
	remote          *RemoteSandbox     // L4：可选，Tier-0 OOM 逃生路径
	persistent      *PersistentSandbox // D4/ADR-0078：Tier2+ 可选持久化沙箱，Available()==false 时按既有链路降级
	goos            string             // "darwin" | "linux" | "windows"
	hwTier          int                // 0 = Tier 0 (8GB) 主线
	newWasmDisabled atomic.Bool
	// activeExecs 追踪所有正在执行的沙箱任务的取消函数，覆盖 Wasm/Container/
	// NativeOS/InProcess/Remote 全部 tier（D-B6-04 修复：原 activeWasm 从未在
	// Execute() 中被写入，Kill* 方法长期是空 map 上的 no-op，且命名/注释均
	// 仅覆盖 Wasm，Container/NativeOS 完全脱离追踪，OOM 压力下无法强制回收）。
	// 统一在 Execute() 单一执行入口处注册/注销，覆盖所有 tier。
	activeExecs map[string]context.CancelFunc
	execSeq     atomic.Uint64
}

func NewSandboxRouter(inProcess *InProcessSandbox, container *ContainerSandbox, wasmtime SandboxProvider, goos string, hwTier int) *SandboxRouter {
	return &SandboxRouter{
		inProcess:   inProcess,
		container:   container,
		wasmtime:    wasmtime,
		goos:        goos,
		hwTier:      hwTier,
		activeExecs: make(map[string]context.CancelFunc),
	}
}

// DisableNewInstances 满足 observability.SandboxController，禁止启动新 Wasm 实例（L1 预警）。
func (r *SandboxRouter) DisableNewInstances(disable bool) {
	r.newWasmDisabled.Store(disable)
}

// KillIdleSandboxes 回收正在执行的沙箱任务（OSMemoryGuard L2 级调用）。
// 覆盖 Wasm/Container/NativeOS/InProcess/Remote 全部 tier（D-B6-04）。
func (r *SandboxRouter) KillIdleSandboxes(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := int64(len(r.activeExecs))
	for k, cancel := range r.activeExecs {
		cancel()
		delete(r.activeExecs, k)
	}
	if count > 0 {
		slog.InfoContext(ctx, "sandbox: killed idle sandbox executions", "count", count)
	}
}

// KillAllNonCritical 回收全部非关键沙箱（OSMemoryGuard L3 临界内存压力调用）。
// 强制终止所有已知的正在执行的沙箱任务，覆盖 Wasm/Container/NativeOS/
// InProcess/Remote 全部 tier（D-B6-04：原实现仅追踪 Wasm 且从未真正写入过
// 追踪表，Container/NativeOS 长期是防御盲区）。
func (r *SandboxRouter) KillAllNonCritical(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := int64(len(r.activeExecs))
	for k, cancel := range r.activeExecs {
		cancel()
		delete(r.activeExecs, k)
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

// WithPersistent 注入 PersistentSandbox（D4/ADR-0078，Tier2+ 可选持久化沙箱）。
// 返回自身，支持链式调用。注入后 SandboxPersistent tier 路由至此，但只有
// persistent.Available()==true 时才会真正被选用；当前恒定为 false（诚实占位，
// 见 sandbox_persistent.go），因此注入本身不改变任何现有行为，属于零风险
// 前置接线。
func (r *SandboxRouter) WithPersistent(persistent *PersistentSandbox) *SandboxRouter {
	r.persistent = persistent
	return r
}

// PersistentAvailable 报告 L4 持久化沙箱当前是否已注入且可用（D4/ADR-0079）。
// 供 ExecEnvelope.PersistentSandboxAvailable 转调，供上游调用方（code_act.go）
// 在构造脚本内容前做出确定性的路由决策。
func (r *SandboxRouter) PersistentAvailable() bool {
	return r.persistent != nil && r.persistent.Available()
}

// RouteByTier 根据已算好的 tier 路由，返回 SandboxProvider。
// 规则与 AssignSandboxTier 保持一致。
// trustTier 用于决定隔离不可用时能否降级。
// 安全规则：trust < Official 且要求 L2/L3 但对应沙箱不可用 → fail-closed 拒绝，不降级到 L1。
func (r *SandboxRouter) RouteByTier(tier types.SandboxTier, trustTier types.TrustTier) (SandboxProvider, error) {
	mustIsolate := trustTier < types.TrustOfficial
	switch tier {
	case types.SandboxNativeOS:
		return r.routeNativeOS()
	case types.SandboxRemote:
		if r.remote != nil {
			return r.remote, nil
		}
		return r.routeWasm(mustIsolate)
	case types.SandboxWasm:
		return r.routeWasm(mustIsolate)
	case types.SandboxContainer:
		return r.routeContainer()
	case types.SandboxPersistent:
		return r.routePersistent()
	default: // InProcess
		return r.inProcess, nil
	}
}

// routeNativeOS 路由 Tier-0 NativeOS（Rust 原生沙箱）；未注入时尝试 Container 兜底，
// 否则 fail-closed（从 RouteByTier 拆出，gocyclo 治理，行为不变）。
func (r *SandboxRouter) routeNativeOS() (SandboxProvider, error) {
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
}

// routeWasm 路由 L2/Wasm 隔离：依次尝试 Wasm→Container→Remote；不可信来源找不到
// 隔离时 fail-closed，可信来源允许降级 InProcess（从 RouteByTier 拆出，
// gocyclo 治理，行为不变；SandboxRemote 未注入 remote 时 fallthrough 至此复用同一逻辑）。
func (r *SandboxRouter) routeWasm(mustIsolate bool) (SandboxProvider, error) {
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
}

// routeContainer 路由 L3/Container 隔离；不可用时尝试 Remote，否则 fail-closed
// （从 RouteByTier 拆出，gocyclo 治理，行为不变）。
func (r *SandboxRouter) routeContainer() (SandboxProvider, error) {
	if r.container != nil {
		return r.container, nil
	}
	if r.remote != nil {
		return r.remote, nil
	}
	return nil, apperr.New(apperr.CodeForbidden, "sandbox: L3/Container required but unavailable; refusing to downgrade")
}

// routePersistent 路由 L4/Persistent 隔离（D4/ADR-0078）：Available()==false 时按
// Container 同等的降级链回退，否则 fail-closed（从 RouteByTier 拆出，
// gocyclo 治理，行为不变）。
func (r *SandboxRouter) routePersistent() (SandboxProvider, error) {
	// D4/ADR-0078：Available()==false 时（当前恒定如此，见
	// sandbox_persistent.go）按 Container 同等的降级链回退，语义与设计
	// 文档"否则保持现状回退到既有 StatefulSession pickle/env 序列化路径"
	// 一致——调用方拿到 Container/Remote 后仍会走原有的一次性进程执行
	// 路径，StatefulSession 的样板注入逻辑不受影响。
	if r.persistent != nil && r.persistent.Available() {
		return r.persistent, nil
	}
	if r.container != nil {
		return r.container, nil
	}
	if r.remote != nil {
		return r.remote, nil
	}
	return nil, apperr.New(apperr.CodeForbidden, "sandbox: L4/Persistent requested but neither persistent backend nor Container/Remote fallback available; refusing to downgrade")
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

	// D-B6-04：统一注册可取消 context，供 KillIdleSandboxes/KillAllNonCritical
	// 在 OOM 压力下强制终止在执行的沙箱任务。Execute 是所有 tier（Wasm/
	// Container/NativeOS/InProcess/Remote）唯一的执行入口，在此处单点注册
	// 即可覆盖全部 tier，无需侵入各 SandboxProvider 具体实现。
	execCtx, cancel := context.WithCancel(ctx)
	key := fmt.Sprintf("%s-%d", tool.Name, r.execSeq.Add(1))
	r.mu.Lock()
	r.activeExecs[key] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.activeExecs, key)
		r.mu.Unlock()
		cancel()
	}()

	res, err := provider.Run(execCtx, spec)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("sandbox run tool %q", tool.Name), err)
	}
	return res, nil
}
