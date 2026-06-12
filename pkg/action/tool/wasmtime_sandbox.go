package tool

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/action"
)

// WasmtimeSandbox 实现 SandboxProvider，利用 Rust Wasmtime 引擎执行 Wasm 字节码。
type WasmtimeSandbox struct {
	workspaceDir string
}

// NewWasmtimeSandbox 初始化 Wasmtime 沙箱。
func NewWasmtimeSandbox(workspaceDir string) *WasmtimeSandbox {
	// 尝试初始化 Wasmtime FFI，失败（如无 dylib）会在 WasmtimeExecute 时报错拦截。
	_ = WasmtimeInit()
	return &WasmtimeSandbox{workspaceDir: workspaceDir}
}

// Level 返回沙箱安全层级 (L2)。
func (s *WasmtimeSandbox) Level() int {
	return 2
}

// Run 执行 Wasm 沙箱调用。
func (s *WasmtimeSandbox) Run(ctx context.Context, spec action.SandboxSpec) (*protocol.ToolResult, error) {
	start := time.Now()

	// quotaMs := spec.CPUQuotaMs
	// if quotaMs == 0 {
	// 	quotaMs = 5000
	// }

	// 从能力推导是否允许网络出站
	networkAllowed := false
	if spec.Capability >= protocol.CapWriteNetwork {
		networkAllowed = true
	}

	// 执行 FFI 调用
	// 注意：Wasm 引擎内存页计算目前传递硬编码 16 (16*64KB = 1MB)，后续可由 cfg 提供
	outJSON, execErr := WasmtimeExecute(
		spec.ScriptBytes,
		string(spec.Input),
		s.workspaceDir,
		16, // maxPages
		networkAllowed,
	)

	latency := time.Since(start).Milliseconds()

	//nolint:nilerr
	if execErr != nil {
		return &protocol.ToolResult{
			Success:   false,
			Error:     execErr.Error(),
			LatencyMs: latency,
		}, nil
	}

	return &protocol.ToolResult{
		Success:   true,
		Output:    []byte(outJSON),
		LatencyMs: latency,
	}, nil
}
