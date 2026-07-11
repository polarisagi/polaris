// Package tool — rust_wasmtime_sandbox.go
//
// Go→Rust FFI 桥接：通过 purego 调用 rust/substrate wasmtime_engine。
//
// 设计原则：
//   - 提供对 Wasmtime 的纯 Go 接口封装
//   - 通过 sync.Once 懒加载共享同一个 dylib
//
// 宿主侧墙钟超时兜底（Batch11 GR-7.1）：Rust 侧 wasmtime_execute 已用 epoch
// interruption 提供墙钟超时，但 epoch 检查点只在 WASM 生成代码边界触发，无法
// 打断已经陷入阻塞态的 host 系统调用（network_allowed=1 时挂起的 TCP
// connect/read）。WasmtimeExecute 因此额外用 goroutine + select + ctx 包装
// 原始 FFI 调用：超过 Rust 侧超时预算的合理余量后，即使该 goroutine 仍卡在
// 阻塞的系统调用里，也会放弃等待、让宿主调用方及时拿回控制权——代价是极端
// 情况下牺牲一个 OS 线程（随底层调用最终返回或进程退出回收，不会无限累积），
// 用一次线程泄漏换取宿主 goroutine 不会无界阻塞（P-1 强制超时 / P-13
// goroutine 泄漏防御）。
package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	sffi "github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// wasmtimeHostTimeoutSlack 是宿主侧兜底超时相对 Rust 侧 timeout_ms 的安全余量：
// 先给 Rust epoch interruption 完整的预算窗口正常返回，超出后再判定为
// "host 系统调用阻塞、epoch 无法打断"场景，由宿主侧强制放弃等待。
const wasmtimeHostTimeoutSlack = 5 * time.Second

// wasmtimeDefaultTimeoutMs 与 Rust 侧 DEFAULT_TIMEOUT_MS（wasmtime_engine.rs）
// 及 internal/sandbox.SandboxSpec.CPUQuotaMs 的既有约定"0 = 默认 5000ms"一致。
const wasmtimeDefaultTimeoutMs = 5000

var (
	wasmtimeOnce sync.Once
	wasmtimeErr  error

	wasmtimeInit       func(outErr *uintptr) int32
	wasmtimePoolInit   func(n int32) int32
	wasmtimePing       func() int32
	wasmtimeExecute    func(wasmBytes *byte, wasmLen uintptr, inputJSON *byte, workspaceDir *byte, maxPages int32, maxFuel uint64, networkAllowed int32, maxOutputBytes int32, timeoutMs uint64, outJSON *uintptr, outJSONLen *uintptr, outErr *uintptr) int32 //nolint:gochecknoglobals // purego 绑定函数指针，sync.Once 幂等懒绑定，架构性必需（同包 wasmtimeInit 等同类声明先例）
	wasmtimeFreeString func(ptr uintptr)
	wasmtimeFreeBytes  func(ptr uintptr, len uintptr)
)

func bindWasmtime() error {
	wasmtimeOnce.Do(func() {
		lib, err := sffi.Load()
		if err != nil {
			wasmtimeErr = err
			return
		}
		purego.RegisterLibFunc(&wasmtimeInit, lib, "wasmtime_init")
		purego.RegisterLibFunc(&wasmtimePoolInit, lib, "wasmtime_pool_init")
		purego.RegisterLibFunc(&wasmtimePing, lib, "wasmtime_ping")
		purego.RegisterLibFunc(&wasmtimeExecute, lib, "wasmtime_execute")
		purego.RegisterLibFunc(&wasmtimeFreeString, lib, "wasmtime_free_string")
		purego.RegisterLibFunc(&wasmtimeFreeBytes, lib, "wasmtime_free_bytes")
	})
	return wasmtimeErr
}

// readAndFreeWasmtimeStr 读取 Rust 分配的 C 字符串并立即 free，返回 Go string。
func readAndFreeWasmtimeStr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var n int
	for {
		b := *(*byte)(unsafe.Pointer(ptr + uintptr(n)))
		if b == 0 {
			break
		}
		n++
	}
	s := string(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), n))
	wasmtimeFreeString(ptr)
	return s
}

// readAndFreeWasmtimeBytes 读取 Rust 分配的字节切片并立即 free，返回 Go byte slice
func readAndFreeWasmtimeBytes(ptr uintptr, length uintptr) []byte {
	if ptr == 0 || length == 0 {
		return nil
	}
	s := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), length)
	// copy slice data since we are going to free the C memory
	b := make([]byte, length)
	copy(b, s)
	wasmtimeFreeBytes(ptr, length)
	return b
}

// WasmtimeInit 初始化全局 Wasmtime Engine
func WasmtimeInit() error {
	if err := bindWasmtime(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "rust_wasmtime: dylib not available", err)
	}
	var outErr uintptr
	rc := wasmtimeInit(&outErr)
	if rc != 0 {
		errStr := readAndFreeWasmtimeStr(outErr)
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("wasmtime_init failed: %s", errStr))
	}
	return nil
}

// WasmtimePoolInit 初始化 Wasmtime 的热池
func WasmtimePoolInit(n int) error {
	if err := bindWasmtime(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "rust_wasmtime: dylib not available", err)
	}
	rc := wasmtimePoolInit(int32(n))
	if rc != 0 {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("wasmtime_pool_init failed: %d", rc))
	}
	return nil
}

// WasmtimePing 探测 Wasmtime 引擎状态
func WasmtimePing() error {
	if err := bindWasmtime(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "rust_wasmtime: dylib not available", err)
	}
	rc := wasmtimePing()
	if rc != 42 { // 42 是 wasmtime_ping 的硬编码预期返回值
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("wasmtime_ping failed: expected 42, got %d", rc))
	}
	return nil
}

// wasmtimeExecResult 承载 runWasmtimeExecuteFFI 的返回值，供 WasmtimeExecute
// 通过 channel 在 goroutine 间传递（Batch11 GR-7.1）。
type wasmtimeExecResult struct {
	json string
	err  error
}

// WasmtimeExecute 执行 WebAssembly 模块并返回 JSON 结果。
// timeoutMs 是 Rust 侧 epoch interruption 墙钟超时预算（Batch11 GR-7.1）；
// <=0 时使用与 internal/sandbox.SandboxSpec.CPUQuotaMs 一致的既有约定默认值
// 5000ms。宿主侧另有 wasmtimeHostTimeoutSlack 兜底（见包注释），覆盖 epoch
// 无法打断的阻塞网络系统调用场景；ctx 被调用方取消时同样会提前返回。
func WasmtimeExecute(ctx context.Context, wasmBytes []byte, inputJSON string, workspaceDir string, maxPages int, networkAllowed bool, fuel int, maxOutputBytes int, timeoutMs int) (string, error) {
	if err := bindWasmtime(); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "rust_wasmtime: dylib not available", err)
	}

	if len(wasmBytes) == 0 {
		return "", apperr.New(apperr.CodeInvalidInput, "empty wasm bytes")
	}

	effectiveTimeoutMs := timeoutMs
	if effectiveTimeoutMs <= 0 {
		effectiveTimeoutMs = wasmtimeDefaultTimeoutMs
	}
	hostTimeout := time.Duration(effectiveTimeoutMs)*time.Millisecond + wasmtimeHostTimeoutSlack

	hostCtx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()

	resultCh := make(chan wasmtimeExecResult, 1)

	// 用 concurrent.SafeGo 而非裸 go func()：统一 panic 恢复 + PanicTotal 指标
	// （HE-1 可观测优先），该 goroutine 在宿主侧超时后可能仍卡在阻塞的 FFI
	// 调用里、独立于 hostCtx 生命周期结束，属于本设计有意为之的"牺牲一次
	// OS 线程换取宿主不被无界阻塞"（见包注释），非泄漏 bug。
	concurrent.SafeGo(ctx, "sandbox.WasmtimeExecute", func(_ context.Context) {
		resultCh <- runWasmtimeExecuteFFI(wasmBytes, inputJSON, workspaceDir, maxPages, networkAllowed, fuel, maxOutputBytes, uint64(effectiveTimeoutMs))
	})

	select {
	case r := <-resultCh:
		return r.json, r.err
	case <-hostCtx.Done():
		return "", apperr.New(apperr.CodeInternal,
			fmt.Sprintf("wasmtime_execute: host-side wall-clock timeout after %v (goroutine abandoned; likely blocked host syscall that epoch interruption cannot preempt, e.g. network I/O)", hostTimeout))
	}
}

// runWasmtimeExecuteFFI 执行实际的 purego FFI 调用（原 WasmtimeExecute 函数体），
// 从 WasmtimeExecute 中拆出以便被 goroutine+select 包装（Batch11 GR-7.1）。
func runWasmtimeExecuteFFI(wasmBytes []byte, inputJSON string, workspaceDir string, maxPages int, networkAllowed bool, fuel int, maxOutputBytes int, timeoutMs uint64) wasmtimeExecResult {
	// 转换为 NUL-terminated C-Strings
	// 复用 rust_native_sandbox.go 中的 goStringToC
	inputCStr := goStringToC(inputJSON)
	var workspaceCStr []byte
	if workspaceDir != "" {
		workspaceCStr = goStringToC(workspaceDir)
	}

	var netAllow int32 = 0
	if networkAllowed {
		netAllow = 1
	}

	var outJSON uintptr
	var outJSONLen uintptr
	var outErr uintptr

	var workspacePtr *byte
	if len(workspaceCStr) > 0 {
		workspacePtr = &workspaceCStr[0]
	}

	rc := wasmtimeExecute(
		&wasmBytes[0],
		uintptr(len(wasmBytes)),
		&inputCStr[0],
		workspacePtr,
		int32(maxPages),
		uint64(fuel),
		netAllow,
		int32(maxOutputBytes),
		timeoutMs,
		&outJSON,
		&outJSONLen,
		&outErr,
	)

	errStr := readAndFreeWasmtimeStr(outErr)
	jsonBytes := readAndFreeWasmtimeBytes(outJSON, outJSONLen)

	if rc != 0 {
		return wasmtimeExecResult{"", apperr.New(apperr.CodeInternal, fmt.Sprintf("wasmtime_execute failed (code %d): %s", rc, errStr))}
	}

	return wasmtimeExecResult{string(jsonBytes), nil}
}
