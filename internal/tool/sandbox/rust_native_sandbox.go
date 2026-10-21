// Package tool — rust_native_sandbox.go
//
// Go→Rust FFI 桥接：通过 purego 调用 rust/substrate 的 V2 沙箱导出函数
// （native_sandbox_exec_v2 / native_sandbox_wrap_argv / native_sandbox_probe_tools）。
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §4.2，ADR-0011
//
// 设计原则：
//   - 此文件是 Go 层的薄封装，不含任何平台特定逻辑（平台逻辑全在 Rust）
//   - Go 侧统一走 V2（CmdRunner 已于 2026-07-02 从 V1 迁移完毕，见 cmd_runner_adapter.go）；
//     Rust 侧保留 V1 FFI 导出（native_sandbox_exec）不删、Go 侧不绑定，理由与重评
//     触发条件见 ADR-0011 决策四，白名单条目在 tools/baselines/deadcode-allowlist.txt。
//     禁止为了让 make ffi-check 转绿而在此补一个空绑定——2026-08-12 出现过一次：
//     补的绑定签名是 5 参而 Rust V1 实际是 3 参（req_json/out_json/out_err），
//     一旦有人调用就是栈错位。门控报未绑定时的正解是删符号或登记白名单，不是造绑定。
//   - Rust 不可用（dylib 缺失/符号未找到）时 fail-closed（返回 error），无 Go 侧降级路径
//   - 通过 sync.Once + ffi.Load() 懒加载，与 Cedar/Surreal 共享同一 dylib 句柄

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	sffi "github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── purego 函数指针（懒绑定）────────────────────────────────────────────────

var (
	nativeOnce sync.Once
	nativeErr  error

	nativeSandboxProbeTools func(outJSON *uintptr, outErr *uintptr) int32
	nativeSandboxFreeString func(ptr uintptr)

	// V2 函数指针（Go 侧唯一执行路径，CmdRunner 已全量迁移；V1 native_sandbox_exec
	// 已于 2026-08-12 从 Rust 侧一并移除，不再存在可绑定的 V1 符号）
	nativeSandboxExecV2   func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
	nativeSandboxWrapArgv func(inputJSON uintptr, outJSON *uintptr, outErr *uintptr) int32
)

func bindNativeSandbox() error {
	nativeOnce.Do(func() {
		lib, err := sffi.Load()
		if err != nil {
			nativeErr = err
			return
		}
		purego.RegisterLibFunc(&nativeSandboxProbeTools, lib, "native_sandbox_probe_tools")
		purego.RegisterLibFunc(&nativeSandboxFreeString, lib, "native_sandbox_free_string")
		// V2（Rust 侧已实现，不存在时 purego 会 panic；用 recover 包裹防启动崩溃）
		func() {
			defer func() { recover() }() //nolint:errcheck
			purego.RegisterLibFunc(&nativeSandboxExecV2, lib, "native_sandbox_exec_v2")
			purego.RegisterLibFunc(&nativeSandboxWrapArgv, lib, "native_sandbox_wrap_argv")
		}()
	})
	return nativeErr
}

// ─── 输入/输出结构体（与 Rust 侧镜像，json 序列化）─────────────────────────

// RustSandboxResponse 与 Rust NativeSandboxResponse 字段对齐。
type RustSandboxResponse struct {
	Output        string `json:"output"`
	ExitCode      int    `json:"exit_code"`
	SandboxMethod string `json:"sandbox_method"`
	MemoryLimited bool   `json:"memory_limited"`
	// NetIsolated true 表示 network_block 已由 seatbelt/bwrap 真实强制（非未经验证的声明）。
	// Rust dispatch.rs 已 fail-closed：SandboxMethod 若为降级方法（bare/namespace/wsl2），
	// 只可能出现在调用方未要求 network_block 的场景，此时 NetIsolated 恒为 false。
	NetIsolated bool `json:"net_isolated"`
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

// ─── V2 公开 API ──────────────────────────────────────────────────────────────

// rustSandboxContextJSON 将 protocol.SandboxContext 序列化为 NUL 终止 JSON 字节切片。
func rustSandboxContextJSON(ctx protocol.SandboxContext) ([]byte, error) {
	b, err := json.Marshal(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rust_native_sandbox_v2: marshal SandboxContext", err)
	}
	return goStringToC(string(b)), nil
}

// withDefaultDenyPaths 调用方未显式给出 DenyPaths / DenyWritePaths 时注入默认清单
// （ADR-0121 决策五、ADR-0122 决策二）。集中在 FFI 入口注入，使所有调用方自动生效。
// 系统目录与持久化落点只进 DenyWritePaths：沙箱对它们只需写保护，若读写全拒会让 bash 本身
// 无法加载。项目内 .git/hooks 与 .git/config 无论调用方是否显式给了清单都会追加——
// 它们是仓库内最易被利用的持久化点，不是调用方可以忘掉的细节。
func withDefaultDenyPaths(ctx protocol.SandboxContext) protocol.SandboxContext {
	var rw, wo []string
	if len(ctx.DenyPaths) == 0 || len(ctx.DenyWritePaths) == 0 {
		rw, wo = policy.SandboxDenyPaths()
	}
	if len(ctx.DenyPaths) == 0 {
		ctx.DenyPaths = mergeDenyPaths(rw)
	}
	if len(ctx.DenyWritePaths) == 0 {
		ctx.DenyWritePaths = mergeDenyPaths(wo)
	}
	ctx.DenyWritePaths = mergeDenyPaths(ctx.DenyWritePaths, projectGitDenyPaths(ctx))
	return ctx
}

// projectGitDenyPaths 从可读写根（AllowedPaths ∪ Workdir）推导 <root>/.git/hooks 与 <root>/.git/config，
// 仅收已存在的路径（bwrap 对不存在的挂载点会失败；Seatbelt 对将来创建的不受保护，属已知缺口）。
func projectGitDenyPaths(ctx protocol.SandboxContext) []string {
	roots := append(append([]string{}, ctx.AllowedPaths...), ctx.Workdir)
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		for _, rel := range []string{".git/hooks", ".git/config"} {
			p := filepath.Join(r, filepath.FromSlash(rel))
			if _, err := os.Lstat(p); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// mergeDenyPaths 按首次出现顺序去重合并。
func mergeDenyPaths(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, g := range groups {
		for _, p := range g {
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// RustSandboxExecV2 通过 Rust FFI V2 执行沙箱命令（run-to-completion）。
// 用于 codeact/skill/hook/builtin 等短生命周期执行。
func RustSandboxExecV2(ctx protocol.SandboxContext, timeoutMs uint64) (*RustSandboxResponse, error) {
	if err := bindNativeSandbox(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rust_native_sandbox_v2: dylib not available", err)
	}
	if nativeSandboxExecV2 == nil {
		return nil, apperr.New(apperr.CodeInternal, "rust_native_sandbox_v2: native_sandbox_exec_v2 symbol not found (rebuild dylib)")
	}
	if timeoutMs > 0 {
		ctx.TimeoutMs = timeoutMs
	}
	ctx = withDefaultDenyPaths(ctx)
	inputCStr, err := rustSandboxContextJSON(ctx)
	if err != nil {
		return nil, err
	}

	var outJSON, outErr uintptr
	code := nativeSandboxExecV2(uintptr(unsafe.Pointer(&inputCStr[0])), &outJSON, &outErr)
	runtime.KeepAlive(inputCStr)

	errStr := readAndFreeRustStr(outErr)
	jsonStr := readAndFreeRustStr(outJSON)

	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox_v2: exec failed (code=%d): %s", code, errStr))
	}
	var resp RustSandboxResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox_v2: unmarshal response: %s", jsonStr), err)
	}
	return &resp, nil
}

// RustSandboxWrapArgv 通过 Rust FFI 获取沙箱封装后的 argv，不启动进程。
// 用于 MCP stdio 长进程：调用方用返回的 Executable+Argv 构建 exec.Cmd 并持有管道。
func RustSandboxWrapArgv(ctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	if err := bindNativeSandbox(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "rust_native_sandbox_v2: dylib not available", err)
	}
	if nativeSandboxWrapArgv == nil {
		return nil, apperr.New(apperr.CodeInternal, "rust_native_sandbox_v2: native_sandbox_wrap_argv symbol not found (rebuild dylib)")
	}
	ctx = withDefaultDenyPaths(ctx)
	inputCStr, err := rustSandboxContextJSON(ctx)
	if err != nil {
		return nil, err
	}

	var outJSON, outErr uintptr
	code := nativeSandboxWrapArgv(uintptr(unsafe.Pointer(&inputCStr[0])), &outJSON, &outErr)
	runtime.KeepAlive(inputCStr)

	errStr := readAndFreeRustStr(outErr)
	jsonStr := readAndFreeRustStr(outJSON)

	if code < 0 {
		return nil, apperr.New(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox_v2: wrap_argv failed (code=%d): %s", code, errStr))
	}
	var result protocol.WrapArgvResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("rust_native_sandbox_v2: unmarshal WrapArgvResult: %s", jsonStr), err)
	}
	return &result, nil
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
