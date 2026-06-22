// Package tool — rust_native_sandbox.go
//
// Go→Rust FFI 桥接：通过 purego 调用 rust/substrate native_sandbox_exec。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2，ADR-0011
//
// 设计原则：
//   - 此文件是 Go 层的薄封装，不含任何平台特定逻辑（平台逻辑全在 Rust）
//   - 调用方透明：与旧 Go native_sandbox 的接口保持一致（NativeSandboxCfg）
//   - Rust 不可用时 fail-closed（返回 error），由 WrapBashCmd 决策降级策略
//   - 通过 sync.Once + ffi.Load() 懒加载，与 Cedar/Surreal 共享同一 dylib 句柄

package sandbox

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	sffi "github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── purego 函数指针（懒绑定）────────────────────────────────────────────────

var (
	nativeOnce sync.Once
	nativeErr  error

	// nativeSandboxExec 执行沙箱命令，返回 JSON 结果。
	nativeSandboxExec func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
	// nativeSandboxProbeTools 探测当前系统沙箱能力与语言运行时。
	nativeSandboxProbeTools func(outJSON *uintptr, outErr *uintptr) int32
	// nativeSandboxFreeString 释放 Rust 分配的 C 字符串。
	nativeSandboxFreeString func(ptr uintptr)
)

func bindNativeSandbox() error {
	nativeOnce.Do(func() {
		lib, err := sffi.Load()
		if err != nil {
			nativeErr = err
			return
		}
		purego.RegisterLibFunc(&nativeSandboxExec, lib, "native_sandbox_exec")
		purego.RegisterLibFunc(&nativeSandboxProbeTools, lib, "native_sandbox_probe_tools")
		purego.RegisterLibFunc(&nativeSandboxFreeString, lib, "native_sandbox_free_string")
	})
	return nativeErr
}

// ─── 输入/输出结构体（与 Rust 侧镜像，json 序列化）─────────────────────────

// rustSandboxRequest 与 Rust NativeSandboxRequest 字段对齐。
type rustSandboxRequest struct {
	Command      string   `json:"command"`
	Workdir      string   `json:"workdir,omitempty"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
	NetworkBlock bool     `json:"network_block"`
	EnvExtra     []string `json:"env_extra,omitempty"`
	TimeoutMs    uint64   `json:"timeout_ms,omitempty"`
	BwrapPath    string   `json:"bwrap_path,omitempty"`
	MaxMemoryMB  uint64   `json:"max_memory_mb,omitempty"`
}

// RustSandboxResponse 与 Rust NativeSandboxResponse 字段对齐。
type RustSandboxResponse struct {
	Output        string `json:"output"`
	ExitCode      int    `json:"exit_code"`
	SandboxMethod string `json:"sandbox_method"`
	MemoryLimited bool   `json:"memory_limited"`
}

// ─── FFI 字符串辅助 ───────────────────────────────────────────────────────────

// goStringToC 将 Go string 转为 NUL-terminated 字节切片（Rust 侧读取用）。
// 调用方必须确保返回的切片在 Rust FFI 调用期间不被 GC 回收（pin 语义）。
func goStringToC(s string) []byte {
	b := make([]byte, len(s)+1)
	copy(b, s)
	b[len(s)] = 0
	return b
}

// readAndFreeRustStr 读取 Rust 分配的 C 字符串并立即 free，返回 Go string。
func readAndFreeRustStr(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	// 从指针读取 NUL-terminated 字符串
	var n int
	for {
		b := *(*byte)(unsafe.Pointer(ptr + uintptr(n)))
		if b == 0 {
			break
		}
		n++
	}
	s := string(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), n))
	nativeSandboxFreeString(ptr)
	return s
}

// ─── 公开 API ─────────────────────────────────────────────────────────────────

// RustSandboxExec 通过 Rust FFI 执行沙箱命令。
// cfg 使用与 Go native_sandbox 相同的 NativeSandboxCfg 结构，调用方无感知切换。
func RustSandboxExec(cfg NativeSandboxCfg, timeoutMs uint64) (*RustSandboxResponse, error) {
	if err := bindNativeSandbox(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			"rust_native_sandbox: dylib not available", err)
	}

	netBlock := cfg.NetworkPolicy == NetworkBlock

	req := rustSandboxRequest{
		Command:      cfg.Command,
		Workdir:      cfg.WorkDir,
		AllowedPaths: cfg.AllowedPaths,
		NetworkBlock: netBlock,
		EnvExtra:     cfg.Env,
		TimeoutMs:    timeoutMs,
		BwrapPath:    cfg.BwrapPath,
		MaxMemoryMB:  cfg.MaxMemoryMB,
	}

	inputJSON, err := json.Marshal(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rust_native_sandbox: marshal request", err)
	}

	// NUL-terminate 供 Rust CStr::from_ptr
	inputCStr := goStringToC(string(inputJSON))

	var outJSON uintptr
	var outErr uintptr

	code := nativeSandboxExec(
		uintptr(unsafe.Pointer(&inputCStr[0])),
		&outJSON,
		&outErr,
	)

	errStr := readAndFreeRustStr(outErr)
	jsonStr := readAndFreeRustStr(outJSON)

	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox: exec failed (code=%d): %s", code, errStr))
	}

	var resp RustSandboxResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox: unmarshal response: %s", jsonStr), err)
	}
	return &resp, nil
}

// RustSandboxProbeTools 调用 Rust 探测当前系统沙箱能力和已安装语言运行时。
// 供 sys_probe 工具和启动诊断使用。
func RustSandboxProbeTools() (map[string]any, error) {
	if err := bindNativeSandbox(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			"rust_native_sandbox: dylib not available", err)
	}

	var outJSON uintptr
	var outErr uintptr

	code := nativeSandboxProbeTools(&outJSON, &outErr)
	errStr := readAndFreeRustStr(outErr)
	jsonStr := readAndFreeRustStr(outJSON)

	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox: probe failed (code=%d): %s", code, errStr))
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rust_native_sandbox: unmarshal probe", err)
	}
	return result, nil
}
