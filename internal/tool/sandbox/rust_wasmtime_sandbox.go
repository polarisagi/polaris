// Package tool — rust_wasmtime_sandbox.go
//
// Go→Rust FFI 桥接：通过 purego 调用 rust/substrate wasmtime_engine。
//
// 设计原则：
//   - 提供对 Wasmtime 的纯 Go 接口封装
//   - 通过 sync.Once 懒加载共享同一个 dylib
package sandbox

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	sffi "github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/pkg/apperr"
)

var (
	wasmtimeOnce sync.Once
	wasmtimeErr  error

	wasmtimeInit       func(outErr *uintptr) int32
	wasmtimePoolInit   func(n int32) int32
	wasmtimePing       func() int32
	wasmtimeExecute    func(wasmBytes *byte, wasmLen uintptr, inputJSON *byte, workspaceDir *byte, maxPages int32, maxFuel uint64, networkAllowed int32, maxOutputBytes int32, outJSON *uintptr, outJSONLen *uintptr, outErr *uintptr) int32
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

// WasmtimeExecute 执行 WebAssembly 模块并返回 JSON 结果
func WasmtimeExecute(wasmBytes []byte, inputJSON string, workspaceDir string, maxPages int, networkAllowed bool, fuel int, maxOutputBytes int) (string, error) {
	if err := bindWasmtime(); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "rust_wasmtime: dylib not available", err)
	}

	if len(wasmBytes) == 0 {
		return "", apperr.New(apperr.CodeInvalidInput, "empty wasm bytes")
	}

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
		&outJSON,
		&outJSONLen,
		&outErr,
	)

	errStr := readAndFreeWasmtimeStr(outErr)
	jsonBytes := readAndFreeWasmtimeBytes(outJSON, outJSONLen)

	if rc != 0 {
		return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("wasmtime_execute failed (code %d): %s", rc, errStr))
	}

	return string(jsonBytes), nil
}
