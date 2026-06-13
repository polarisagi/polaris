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
	mockProxy    *action.MockProxy
}

// NewWasmtimeSandbox 初始化 Wasmtime 沙箱。
func NewWasmtimeSandbox(workspaceDir string, mockProxy *action.MockProxy) *WasmtimeSandbox {
	// 尝试初始化 Wasmtime FFI，失败（如无 dylib）会在 WasmtimeExecute 时报错拦截。
	_ = WasmtimeInit()
	// 初始化 Wasmtime Warm-Pool，预热 5 个实例
	_ = WasmtimePoolInit(5)
	return &WasmtimeSandbox{
		workspaceDir: workspaceDir,
		mockProxy:    mockProxy,
	}
}

// Level 返回沙箱安全层级 (L2)。
func (s *WasmtimeSandbox) Level() int {
	return 2
}

// Run 执行 Wasm 沙箱调用。
func (s *WasmtimeSandbox) Run(ctx context.Context, spec action.SandboxSpec) (*protocol.ToolResult, error) {
	if spec.DryRunMode && s.mockProxy != nil {
		// MockProxy 拦截 DryRun 执行
		return s.mockProxy.Execute(ctx, spec.ToolName, spec.Input)
	}

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

	// 动态计算配额
	quota := CalculateWasmQuota(spec.SystemTier, spec.TaintLevel)

	// 执行 FFI 调用
	outJSON, execErr := WasmtimeExecute(
		spec.ScriptBytes,
		string(spec.Input),
		s.workspaceDir,
		quota.MemoryPages,
		networkAllowed,
		quota.Fuel,
		quota.MaxMounts,
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
