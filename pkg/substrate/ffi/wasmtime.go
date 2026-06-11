package ffi

import (
	"context"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

var (
	wasmtimeOnce sync.Once
	wasmtimeErr  error

	wasmtimeInit       func(outErr *uintptr) int32
	wasmtimePing       func() int32
	wasmtimeFreeString func(ptr uintptr)
	wasmtimeExecute    func(wasmBytes uintptr, wasmLen uintptr, inputJson uintptr, workspaceDir uintptr, maxPages int32, maxFuel uint64, outJson *uintptr, outErr *uintptr) int32
)

func bindWasmtime() error {
	wasmtimeOnce.Do(func() {
		lib, err := Load()
		if err != nil {
			wasmtimeErr = err
			return
		}
		purego.RegisterLibFunc(&wasmtimeInit, lib, "wasmtime_init")
		purego.RegisterLibFunc(&wasmtimePing, lib, "wasmtime_ping")
		purego.RegisterLibFunc(&wasmtimeFreeString, lib, "wasmtime_free_string")
		purego.RegisterLibFunc(&wasmtimeExecute, lib, "wasmtime_execute")
	})
	return wasmtimeErr
}

func readWasmtimeCStringAndFree(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	s := wasmtimeGoStringFromPtr(ptr)
	wasmtimeFreeString(ptr)
	return s
}

func wasmtimeGoStringFromPtr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var n uintptr
	for {
		b := *(*byte)(unsafe.Pointer(ptr + n))
		if b == 0 {
			break
		}
		n++
	}
	if n == 0 {
		return ""
	}
	bytes := make([]byte, n)
	for i := uintptr(0); i < n; i++ {
		bytes[i] = *(*byte)(unsafe.Pointer(ptr + i))
	}
	return string(bytes)
}

// WasmtimeEngine 封装 Wasmtime 引擎的 FFI 调用。
type WasmtimeEngine struct{}

func NewWasmtimeEngine() *WasmtimeEngine {
	return &WasmtimeEngine{}
}

// Init 初始化 Wasmtime 全局引擎
func (e *WasmtimeEngine) Init() error {
	if err := bindWasmtime(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "wasmtime load lib", err)
	}
	var outErr uintptr
	rc := wasmtimeInit(&outErr)
	if rc != 0 {
		msg := readWasmtimeCStringAndFree(outErr)
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("wasmtime_init failed: %s", msg))
	}
	if outErr != 0 {
		wasmtimeFreeString(outErr)
	}
	return nil
}

// Ping 测试 FFI 连通性，预期返回 42
func (e *WasmtimeEngine) Ping() int {
	if err := bindWasmtime(); err != nil {
		return -1
	}
	return int(wasmtimePing())
}

// ExecuteOptions 封装 Wasmtime 执行参数
type ExecuteOptions struct {
	Input        string
	WorkspaceDir string
	MaxPages     int32
	MaxFuel      uint64
}

// Execute 执行 Wasm 组件
func (e *WasmtimeEngine) Execute(ctx context.Context, wasmBytes []byte, opts ExecuteOptions) (string, error) {
	if err := bindWasmtime(); err != nil {
		return "", err
	}

	if len(wasmBytes) == 0 {
		return "", perrors.New(perrors.CodeInternal, "empty wasm bytes")
	}

	var inputCString uintptr
	if opts.Input != "" {
		inputBytes := append([]byte(opts.Input), 0)
		inputCString = uintptr(unsafe.Pointer(&inputBytes[0]))
	}

	var workspaceCString uintptr
	if opts.WorkspaceDir != "" {
		workspaceBytes := append([]byte(opts.WorkspaceDir), 0)
		workspaceCString = uintptr(unsafe.Pointer(&workspaceBytes[0]))
	}

	var outJson uintptr
	var outErr uintptr

	rc := wasmtimeExecute(
		uintptr(unsafe.Pointer(&wasmBytes[0])),
		uintptr(len(wasmBytes)),
		inputCString,
		workspaceCString,
		opts.MaxPages,
		opts.MaxFuel,
		&outJson,
		&outErr,
	)

	if rc != 0 {
		msg := readWasmtimeCStringAndFree(outErr)
		if outJson != 0 {
			wasmtimeFreeString(outJson)
		}
		return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("wasmtime_execute failed (code %d): %s", rc, msg))
	}

	result := readWasmtimeCStringAndFree(outJson)
	if outErr != 0 {
		wasmtimeFreeString(outErr)
	}
	return result, nil
}
