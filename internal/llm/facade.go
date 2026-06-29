package llm

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// LLMFacade LLM 层对外统一接口。
//
// 问题背景：
//
//	当前 llm.ProviderRegistry 和 llm.InferenceRouter 被多个上层模块直接持有：
//	gateway/server 持有 *llm.ProviderRegistry + *llm.InferenceRouter，
//	agent/agent.go 持有 protocol.Provider（已是接口，相对合理）。
//	任何 llm 包内部重构都影响 server.go。
//
// 解决方案：
//   - LLMFacade 是 llm 包对外的统一入口接口
//   - 上层模块（gateway/server、agent、swarm）依赖此接口，不直接持有具体 struct
//   - ProviderRegistry 热重载、InferenceRouter 熔断降级对外透明
//
// @consumer: gateway/server/server.go, agent/agent.go, swarm/orchestrator/orchestrator.go
// @producer: llm.InferenceRouter（由 cli.go/bootstrap 构造注入）
type LLMFacade interface {
	// Infer 同步推理（非流式）。
	// opts 可覆盖 model/temperature/maxTokens 等参数。
	Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error)

	// StreamInfer 流式推理，返回事件 channel（ctx 取消时 channel 关闭）。
	StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error)

	// PickProvider 按角色（"default"/"vlm"/"code"/"embed"）选取最优 Provider。
	// 供需要直接操作 Provider 的模块（如 channel 适配器）使用。
	PickProvider(role string) protocol.Provider

	// Register 热注册一个 LLM Provider（provider_catalog 表变更时调用）。
	Register(name, displayName string, p protocol.Provider)

	// Unregister 热卸载一个 LLM Provider。
	Unregister(name string)
}
