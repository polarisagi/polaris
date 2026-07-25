// Package sandbox — argv_wrapper.go
//
// ArgvWrapper 抽象"获取沙箱封装后的 argv/env，调用方自行构建 exec.Cmd 并持有
// 管道"这一能力，区别于 CmdRunner（cmd_runner.go）的 run-to-completion 语义。
//
// 设计原因：与 CmdRunner 同样的包循环问题——internal/sandbox 不能直接 import
// internal/tool/sandbox（Rust FFI purego 绑定所在包）。真实实现由
// internal/tool/sandbox.RustSandboxWrapArgv 提供（内部调用 Rust
// native_sandbox_wrap_argv），经由 cmd/polaris 装配的适配器注入。
//
// 用途：PersistentSandbox（D4/ADR-0079）需要长驻解释器进程的 stdin/stdout
// 管道贯穿多次调用，CmdRunner"运行到完成才返回"的语义无法满足，因此需要
// 这个独立接口。已有先例：internal/extension/mcp/mcp_client.go 的
// buildSandboxedMCPCmd 用同一底层 Rust 能力为 MCP stdio 长进程做同样的事，
// 只是那里直接依赖 internal/tool/sandbox（extension/mcp 与 internal/sandbox
// 不构成循环），而 internal/sandbox 自身不能这样做。

package sandbox

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ArgvWrapper 返回沙箱封装后的可执行文件路径 + 参数 + 环境变量，不启动进程。
type ArgvWrapper interface {
	// WrapArgv 失败时返回 error；调用方必须 fail-closed 拒绝以裸 exec 方式启动
	// （不得在 wrap 失败时静默降级为无沙箱执行）。
	WrapArgv(ctx context.Context, sctx protocol.SandboxContext) (*protocol.WrapArgvResult, error)
}
