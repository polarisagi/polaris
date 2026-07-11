package sandbox

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

// WasmtimeSandbox 实现 SandboxProvider，利用 Rust Wasmtime 引擎执行 Wasm 字节码。
type WasmtimeSandbox struct {
	workspaceDir string
}

// NewWasmtimeSandbox 初始化 Wasmtime 沙箱。
func NewWasmtimeSandbox(workspaceDir string) *WasmtimeSandbox {
	// 尝试初始化 Wasmtime FFI，失败（如无 dylib）会在 WasmtimeExecute 时报错拦截。
	_ = WasmtimeInit()
	// 初始化 Wasmtime Warm-Pool，预热 5 个实例
	_ = WasmtimePoolInit(5)
	return &WasmtimeSandbox{
		workspaceDir: workspaceDir,
	}
}

// Level 返回沙箱安全层级 (L2)。
func (s *WasmtimeSandbox) Level() int {
	return 2
}

// Run 执行 Wasm 沙箱调用。
func (s *WasmtimeSandbox) Run(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
	if spec.DryRunMode {
		// Wasm 模式下直接返回 Mocked Result
		outJSON := `{"mocked": true, "tool": "` + spec.ToolName + `"}`
		return &types.ToolResult{
			Success: true,
			Output:  []byte(outJSON),
		}, nil
	}

	start := time.Now()

	// CPUQuotaMs 作为 WasmtimeExecute 的墙钟超时预算（Batch11 GR-7.1，此前
	// 该字段在此处一直被注释掉、从未真正传给 FFI 调用——是"意图已写下但未
	// 接线"的死代码，WasmtimeExecute 内部对 <=0 有默认值兜底，此处补上）。
	quotaMs := spec.CPUQuotaMs

	// 从能力推导是否允许网络出站
	networkAllowed := spec.Capability >= types.CapWriteNetwork

	// 动态计算配额
	quota := CalculateWasmQuota(spec.SystemTier, spec.TaintLevel)

	// 执行 FFI 调用
	outJSON, execErr := WasmtimeExecute(
		ctx,
		spec.ScriptBytes,
		string(spec.Input),
		s.workspaceDir,
		quota.MemoryPages,
		networkAllowed,
		quota.Fuel,
		10*1024*1024,
		quotaMs,
	)

	latency := time.Since(start).Milliseconds()

	//nolint:nilerr
	if execErr != nil {
		return &types.ToolResult{
			Success:   false,
			Error:     execErr.Error(),
			LatencyMs: latency,
		}, nil
	}

	return &types.ToolResult{
		Success:   true,
		Output:    []byte(outJSON),
		LatencyMs: latency,
	}, nil
}
