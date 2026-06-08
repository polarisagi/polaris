package action

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// WasmRunnerAdapter 将 *WazeroRuntime 适配为 skill.WasmRunner 接口。
// 避免 pkg/cognition/skill 直接依赖 pkg/action 的具体类型。
// 在 cmd/polaris/main.go 中构造并注入到 WasmSkillExecutor。
type WasmRunnerAdapter struct {
	rt *WazeroRuntime
}

// NewWasmRunnerAdapter 包装 WazeroRuntime。
func NewWasmRunnerAdapter(rt *WazeroRuntime) *WasmRunnerAdapter {
	return &WasmRunnerAdapter{rt: rt}
}

// RunWasm 实现 skill.WasmRunner 接口，将调用转发到 WazeroRuntime.ExecuteTool。
func (a *WasmRunnerAdapter) RunWasm(ctx context.Context, skillName string, wasmBytes []byte, input []byte) ([]byte, error) {
	cfg := &ExecuteConfig{
		Capability:  1, // M7 L1 InProcess 等级
		SandboxTier: 2, // Wasm L2
		CPUQuotaMs:  5000,
		WasmBytes:   wasmBytes,
	}
	result, err := a.rt.ExecuteTool(ctx, skillName, input, cfg, protocol.TaintNone)
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}
