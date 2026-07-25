// Package sandbox — argv_wrapper_adapter.go
//
// RustArgvWrapper 实现 internal/sandbox.ArgvWrapper 接口，以 RustSandboxWrapArgv
// （本包 rust_native_sandbox.go）为后端。
//
// 调用链：PersistentSandbox（internal/sandbox，D4/ADR-0079）
//           → ArgvWrapper 接口
//           → RustArgvWrapper（此文件）
//           → RustSandboxWrapArgv → native_sandbox_wrap_argv (Rust FFI)
//
// 启动时由 cmd/polaris/boot_tools.go 注入到 NewPersistentSandbox。

package sandbox

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	isandbox "github.com/polarisagi/polaris/internal/sandbox"
)

// RustArgvWrapper 实现 isandbox.ArgvWrapper，无状态，可安全并发使用。
type RustArgvWrapper struct{}

// NewRustArgvWrapper 构造默认实现。
func NewRustArgvWrapper() *RustArgvWrapper { return &RustArgvWrapper{} }

// WrapArgv 委托给 RustSandboxWrapArgv；ctx 参数当前未被 Rust FFI 调用消费
// （V2 FFI 本身不支持取消，超时由调用方在拿到 argv 后自行为 exec.Cmd 设置），
// 保留在签名中是为了满足 isandbox.ArgvWrapper 接口契约、也为未来 FFI 支持
// 取消语义时留出扩展空间。
func (RustArgvWrapper) WrapArgv(_ context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error) {
	return RustSandboxWrapArgv(sctx)
}

var _ isandbox.ArgvWrapper = (*RustArgvWrapper)(nil)
