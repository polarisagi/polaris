// Package ffi 提供 Polaris→Rust substrate dylib 的统一加载与 ABI 校验。
// 设计依据: docs/arch/decisions/ADR-0011-cgo-to-purego-migration.md
//
// 调用方:
//   - internal/security/policy/cedar_ffi.go（Cedar 4 函数）
//   - internal/store/surreal_store.go（SurrealDB 21 函数）
//   - internal/tool/sandbox/rust_native_sandbox.go（native_sandbox 3 函数）
//
// 加载语义:
//   - sync.Once 幂等：多调用方共享同一 dylib 句柄
//   - fail-fast：ABI major 不匹配 → panic（防 silent drift）
//   - 路径回退：env 覆盖 → bin 同级 lib → 多级 dev 模式相对路径
package ffi

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/ebitengine/purego"
)

// ExpectedABIMajor 是 Go 端期望的 ABI 主版本。
// Rust 侧 substrate_abi_version() 返回 (major<<16)|minor；major 不匹配 → panic。
// 升级 ABI 同步修改：rust/substrate/src/lib.rs SUBSTRATE_ABI_MAJOR + 此常量。
const ExpectedABIMajor uint16 = 1
const ExpectedABIMinor uint16 = 1

var (
	libHandle uintptr
	loadOnce  sync.Once
	loadErr   error
)

// dylibName 按平台返回 substrate 动态库文件名。
func dylibName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libsubstrate.dylib"
	case "windows":
		return "substrate.dll"
	default: // linux 与其他类 Unix
		return "libsubstrate.so"
	}
}

// libCandidatePaths 返回按优先级排序的候选路径列表。
// 1. POLARIS_SUBSTRATE_LIB 环境变量显式覆盖（最高优先级，CI/容器场景）
// 2. 可执行文件同级 lib/ 目录（生产部署 by Makefile bundling）
// 3. dev 模式：cargo build 默认输出（多级相对路径覆盖不同 cwd）
func libCandidatePaths() []string {
	name := dylibName()
	paths := []string{}
	if env := os.Getenv("POLARIS_SUBSTRATE_LIB"); env != "" {
		paths = append(paths, env)
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "lib", name))
	}
	// dev 模式：测试 cwd 可能在 pkg/<X>/<Y>/，逐级回退到仓库根
	devRel := filepath.Join("rust", "substrate", "target", "release", name)
	for _, prefix := range []string{".", "..", "../..", "../../..", "../../../.."} {
		paths = append(paths, filepath.Join(prefix, devRel))
	}
	return paths
}

// Load 加载 substrate dylib 并校验 ABI 版本，返回库句柄。
// 幂等：同一进程多次调用返回同一句柄。
// 失败：返回 error；ABI major 不匹配 → panic（不可恢复，必须重建 dylib）。
func Load() (uintptr, error) {
	loadOnce.Do(func() {
		libHandle, loadErr = doLoad()
	})
	return libHandle, loadErr
}

func doLoad() (uintptr, error) {
	var lastErr error
	for _, path := range libCandidatePaths() {
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			lastErr = err
			continue
		}
		h, err := dlopen(abs)
		if err != nil {
			lastErr = err
			continue
		}
		if err := verifyABI(h); err != nil {
			return 0, fmt.Errorf("doLoad: %w", err)
		}
		return h, nil
	}
	if lastErr == nil {
		lastErr = apperr.New(apperr.CodeInternal, "no candidate paths matched")
	}
	return 0, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("substrate dylib not found (set POLARIS_SUBSTRATE_LIB or run `make rust-build`); last error: %v", lastErr), lastErr)
}

func verifyABI(lib uintptr) error {
	var ver func() uint32
	purego.RegisterLibFunc(&ver, lib, "substrate_abi_version")
	got := ver()
	gotMajor := uint16(got >> 16)
	if gotMajor != ExpectedABIMajor {
		// ABI major 不匹配是不可恢复错误：Rust 与 Go 编译时不同步。
		// panic 立即崩溃，防止后续 FFI 调用产生 silent corruption。
		panic(fmt.Sprintf(
			"substrate ABI mismatch: want major=%d got=%d (raw=0x%08x); rebuild Rust dylib (`make rust-build`)",
			ExpectedABIMajor, gotMajor, got,
		))
	}

	gotMinor := uint16(got & 0xFFFF)
	if gotMinor != ExpectedABIMinor {
		slog.Warn("substrate ABI minor mismatch — rebuild recommended",
			"want_minor", ExpectedABIMinor, "got_minor", gotMinor, "raw", got)
	}

	return nil
}
