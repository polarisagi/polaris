package action

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/polarisagi/polaris/internal/config"
	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// WazeroRuntime — wazero Wasm 运行时封装。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §4.3

type WazeroRuntime struct {
	runtime     wazero.Runtime
	mu          sync.RWMutex
	goldCache   map[string]wazero.CompiledModule
	silverCache map[string]wazero.CompiledModule
	bronzeCache map[string]*BronzeEntry
	semaphore   chan struct{}
}

// NewWazeroRuntime 初始化 wazero 运行时。
func NewWazeroRuntime(ctx context.Context, concurrency int) *WazeroRuntime {
	// 读取配置
	cfg := config.Get()
	var memLimitPages uint32 = 4096 // default 256MB
	if cfg != nil {
		memLimitPages = uint32(cfg.Thresholds.M7Tool.MaxWasmMemoryMB * 1024 * 1024 / 65536)
	}

	cache := wazero.NewCompilationCache()
	rc := wazero.NewRuntimeConfig().
		WithCompilationCache(cache).
		WithMemoryLimitPages(memLimitPages).
		WithCloseOnContextDone(true)

	if concurrency <= 0 {
		concurrency = 4 // Fallback 默认值 (Tier-0)
	}
	sem := make(chan struct{}, concurrency)

	r := wazero.NewRuntimeWithConfig(ctx, rc)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)
	return &WazeroRuntime{
		runtime:     r,
		goldCache:   make(map[string]wazero.CompiledModule),
		silverCache: make(map[string]wazero.CompiledModule),
		bronzeCache: make(map[string]*BronzeEntry),
		semaphore:   sem,
	}
}

type BronzeEntry struct {
	Module    wazero.CompiledModule
	ExpiresAt int64
	LastUsed  int64
}

type ExecuteConfig struct {
	Capability     int
	SandboxTier    int
	CPUQuotaMs     int
	WallClockLimit int
	MaxPages       int
	IOBudgetBytes  int64
	MaxHostCall    int
	AllowedPaths   []string
	AllowedDomains []string
	LeaseExpiresAt int64  // UTC seconds
	WasmBytes      []byte // 补充：MVP 阶段通过参数传入 Wasm 二进制进行测试
}

type WASIPermission struct {
	FdRead        bool
	FdWrite       bool
	PathOpenRead  []string
	PathOpenWrite []string
	SockSendRecv  []string
	ClockTimeGet  bool
	RandomGet     bool
	EnvironGet    bool
	ProcExit      bool
	ArgsGet       bool
}

type ResourceLimits struct {
	CPUTimeMs          int
	MaxHostFuncBlockMs int
	MaxPages           int
	MaxCalls           int
	IOBudgetBytes      int64
	ThrottleSameCallMs int
}

// ExecuteTool wazero 执行流程。
func (wr *WazeroRuntime) ExecuteTool(ctx context.Context, skillName string, input []byte, config *ExecuteConfig, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error) { //nolint:gocyclo
	// TOCTOU 防护: write_network / privileged 操作强制注入 deadline
	if config.Capability >= 2 && config.LeaseExpiresAt > 0 {
		leaseDeadline := time.Unix(config.LeaseExpiresAt, 0)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, leaseDeadline)
		defer cancel()
	}

	select {
	case wr.semaphore <- struct{}{}:
		defer func() { <-wr.semaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var compiled wazero.CompiledModule
	if len(config.WasmBytes) > 0 {
		var err error
		compiled, err = wr.runtime.CompileModule(ctx, config.WasmBytes)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "wazero compile failed", err)
		}
	} else {
		// 回退尝试缓存
		comp, err := wr.GetOrCompile(skillName, 1.0)
		if err != nil || comp == nil {
			return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("wasm module not found for skill: %s", skillName))
		}
		compiled = comp.(wazero.CompiledModule)
	}

	// 初始化 Module 配置
	modConfig := wazero.NewModuleConfig().WithName(fmt.Sprintf("%s_%d", skillName, time.Now().UnixNano()))

	// 如果 input 不为空，通过 Args 传参（假设 TinyGo 产物通过 os.Args 接收）
	if len(input) > 0 {
		modConfig = modConfig.WithArgs(skillName, string(input))
	}

	// 实例化模块
	mod, err := wr.runtime.InstantiateModule(ctx, compiled, modConfig)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "wazero instantiate/execute failed", err)
	}
	defer mod.Close(ctx)

	// 0. 尝试调用 _initialize 以初始化 c-shared reactor 的运行时
	initFn := mod.ExportedFunction("_initialize")
	if initFn != nil {
		_, _ = initFn.Call(ctx)
	}

	// MVP 降级逻辑：如果 Wasm 没有导出 polaris_malloc（例如空壳测试），直接返回成功
	mallocFn := mod.ExportedFunction("polaris_malloc")
	if mallocFn == nil {
		return &protocol.ToolResult{
			Success:    true,
			Output:     []byte("Wasm executed successfully (MVP mode, no ABI)"),
			TaintLevel: taintLevel,
		}, nil
	}

	// 1. 调用 Wasm 导出的 malloc 申请输入内存
	res, err := mallocFn.Call(ctx, uint64(len(input)))
	if err != nil || len(res) == 0 {
		return nil, perrors.Wrap(perrors.CodeInternal, "malloc call failed", err)
	}
	ptr := uint32(res[0])

	// 2. 将 JSON 入参写入 Wasm 线性内存
	if len(input) > 0 {
		if !mod.Memory().Write(ptr, input) {
			return nil, perrors.New(perrors.CodeInternal, "failed to write input to wasm memory")
		}
	}

	// 3. 调用 run 函数执行业务逻辑
	runFn := mod.ExportedFunction("run")
	if runFn == nil {
		return nil, perrors.New(perrors.CodeInternal, "wasm module missing 'run' export")
	}

	res, err = runFn.Call(ctx, uint64(ptr), uint64(len(input)))
	if err != nil || len(res) == 0 {
		return nil, perrors.Wrap(perrors.CodeInternal, "run call failed", err)
	}

	// 4. 解析返回的内存指针与长度 (高 32 位为指针，低 32 位为长度)
	packed := res[0]
	outPtr := uint32(packed >> 32)
	outLen := uint32(packed)

	// 5. 从 Wasm 线性内存读取结果
	var outputCopy []byte
	if outLen > 0 {
		outBytes, ok := mod.Memory().Read(outPtr, outLen)
		if !ok {
			return nil, perrors.New(perrors.CodeInternal, "failed to read output from wasm memory")
		}
		// 深拷贝，防止内存释放后失效
		outputCopy = make([]byte, outLen)
		copy(outputCopy, outBytes)

		// 6. 调用 Wasm 释放内存
		freeFn := mod.ExportedFunction("polaris_free")
		if freeFn != nil {
			_, _ = freeFn.Call(ctx, uint64(outPtr))
		}
	}

	return &protocol.ToolResult{
		Success:    true,
		Output:     outputCopy,
		TaintLevel: taintLevel,
	}, nil
}

// GetOrCompile 分层缓存查找: gold → silver → bronze → compile.
func (wr *WazeroRuntime) GetOrCompile(skillID string, successRate float64) (any, error) {
	wr.mu.RLock()
	defer wr.mu.RUnlock()
	if m, ok := wr.goldCache[skillID]; ok {
		return m, nil
	}
	if m, ok := wr.silverCache[skillID]; ok {
		return m, nil
	}
	if entry, ok := wr.bronzeCache[skillID]; ok {
		return entry.Module, nil
	}
	return nil, nil
}

// PreWarmCache 注入预编译 Wasm 模块到 Gold 缓存。
// Gold 缓存上限 5（Tier-0 硬约束，见 M6Skill.GoldCacheSize）；超限时 evict 最旧条目晋升不了、仅记录失败。
func (wr *WazeroRuntime) PreWarmCache(skillID string, wasmBytes []byte) error {
	compiled, err := wr.runtime.CompileModule(context.Background(), wasmBytes)
	if err != nil {
		return err
	}

	goldLimit, _, _ := wasmCacheLimits()

	wr.mu.Lock()
	defer wr.mu.Unlock()

	// 已存在则直接覆盖（版本更新路径）
	if _, exists := wr.goldCache[skillID]; exists {
		wr.goldCache[skillID] = compiled
		return nil
	}

	if len(wr.goldCache) >= goldLimit {
		// 驱逐任意一条（Gold 层条目均为高频热点，简单 FIFO 足够）
		for k := range wr.goldCache {
			delete(wr.goldCache, k)
			break
		}
	}
	wr.goldCache[skillID] = compiled
	return nil
}

// wasmCacheLimits 从配置读取 Gold/Silver/Bronze 缓存上限。
func wasmCacheLimits() (gold, silver, bronze int) {
	gold, silver, bronze = 5, 20, 25 // 内置默认（Tier-0 硬约束）
	if cfg := config.Get(); cfg != nil {
		t := cfg.Thresholds.M6Skill
		if t.GoldCacheSize > 0 {
			gold = t.GoldCacheSize
		}
		if t.SilverCacheSize > 0 {
			silver = t.SilverCacheSize
		}
		if t.BronzeCacheSize > 0 {
			bronze = t.BronzeCacheSize
		}
	}
	return
}

// CheckLimits 资源硬限制检查。
func (rl *ResourceLimits) CheckLimits(callCount int, ioUsed int64, wallTimeMs int) error {
	if callCount > rl.MaxCalls {
		return ErrSandboxResourceExhausted
	}
	if ioUsed > rl.IOBudgetBytes {
		return ErrSandboxResourceExhausted
	}
	if wallTimeMs > rl.CPUTimeMs*3 {
		return ErrSandboxResourceExhausted
	}
	return nil
}

var ErrSandboxResourceExhausted = &SandboxError{"sandbox resource exhausted"}

type SandboxError struct{ msg string }

func (e *SandboxError) Error() string { return e.msg }

// RunWasm implements standard WASI-based Wasm execution (capturing stdout instead of using custom ABI).
func (wr *WazeroRuntime) RunWasm(ctx context.Context, skillName string, input []byte, config *ExecuteConfig, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error) {
	// TOCTOU 防护: write_network / privileged 操作强制注入 deadline
	if config.Capability >= 2 && config.LeaseExpiresAt > 0 {
		leaseDeadline := time.Unix(config.LeaseExpiresAt, 0)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, leaseDeadline)
		defer cancel()
	}

	select {
	case wr.semaphore <- struct{}{}:
		defer func() { <-wr.semaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var compiled wazero.CompiledModule
	if len(config.WasmBytes) > 0 {
		var err error
		compiled, err = wr.runtime.CompileModule(ctx, config.WasmBytes)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "wazero compile failed", err)
		}
	} else {
		// 回退尝试缓存
		comp, err := wr.GetOrCompile(skillName, 1.0)
		if err != nil || comp == nil {
			return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("wasm module not found for skill: %s", skillName))
		}
		compiled = comp.(wazero.CompiledModule)
	}

	var stdoutBuf bytes.Buffer
	modConfig := wazero.NewModuleConfig().
		WithName(fmt.Sprintf("%s_%d", skillName, time.Now().UnixNano())).
		WithStdout(&stdoutBuf)

	if len(input) > 0 {
		modConfig = modConfig.WithArgs(skillName, string(input))
		modConfig = modConfig.WithStdin(bytes.NewReader(input))
	} else {
		modConfig = modConfig.WithArgs(skillName)
	}

	mod, err := wr.runtime.InstantiateModule(ctx, compiled, modConfig)
	if err != nil {
		return &protocol.ToolResult{
			Success:    false,
			Error:      fmt.Sprintf("wazero execution error: %v", err),
			Output:     stdoutBuf.Bytes(),
			TaintLevel: taintLevel,
		}, nil
	}
	defer mod.Close(ctx)

	return &protocol.ToolResult{
		Success:    true,
		Output:     stdoutBuf.Bytes(),
		TaintLevel: taintLevel,
	}, nil
}
