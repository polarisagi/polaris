package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// ── M1 LLM 调用埋点 ─────────────────────────────────────────────────────────

// RecordLLMCall 记录单次 LLM 调用。
// provider: 内部 Provider 注册名（如 "deepseek-v4"）
// model: 响应中实际使用的 model ID（resp.Model）
// status: "success" | "error" | "failover"
// latencyMs: 调用端到端耗时（ms）
// inputTokens / outputTokens / cacheHitTokens: 来自 protocol.ProviderResponse.Usage
// costUSD: 本次调用费用（USD），由调用方按 Capabilities().CostPer1K* 计算
//
// 调用方: pkg/substrate/inference/router.go Infer() / failover()
func RecordLLMCall(
	ctx context.Context,
	provider, model, status string,
	latencyMs float64,
	inputTokens, outputTokens, cacheHitTokens int,
	costUSD float64,
) {
	// CardinalityGuard: model 可能随 Provider 更新快速增长，须限制
	model = globalCardinalityGuard.Allow(model)

	if instrLLMCallsTotal != nil {
		instrLLMCallsTotal.Add(ctx, 1,
			metric.WithAttributes(attrProvider(provider), attrModel(model), attrStatus(status)))
	}
	if instrLLMLatencyMs != nil {
		instrLLMLatencyMs.Record(ctx, latencyMs,
			metric.WithAttributes(attrModel(model)))
	}
	if instrTokensTotal != nil {
		instrTokensTotal.Add(ctx, int64(inputTokens),
			metric.WithAttributes(attrType("input")))
		instrTokensTotal.Add(ctx, int64(outputTokens),
			metric.WithAttributes(attrType("output")))
		if cacheHitTokens > 0 {
			instrTokensTotal.Add(ctx, int64(cacheHitTokens),
				metric.WithAttributes(attrType("cache_hit")))
		}
	}
	if instrLLMCacheHitRate != nil && (inputTokens+cacheHitTokens) > 0 {
		hitRate := float64(cacheHitTokens) / float64(inputTokens+cacheHitTokens)
		instrLLMCacheHitRate.Record(ctx, hitRate,
			metric.WithAttributes(attrProvider(provider), attrModel(model)))
	}
	if instrAPIcostUSD != nil && costUSD > 0 {
		instrAPIcostUSD.Add(ctx, costUSD,
			metric.WithAttributes(attrProvider(provider), attrModel(model), attrCallType("llm")))
	}
}

// IncrBurnStage3 记录 TokenBurnRate Stage3 FULLSTOP 触发（边沿计数，每次触发调用一次）。
// 调用方: pkg/substrate/inference/router.go 或 M11 KillSwitch 触发点。
func IncrBurnStage3() {
	if instrBurnStage3Total != nil {
		instrBurnStage3Total.Add(context.Background(), 1)
	}
}

func IncrEventBufferDrainTimeout(count int64) {
	if instrEventBufferDrainTimeoutTotal != nil {
		instrEventBufferDrainTimeoutTotal.Add(context.Background(), count)
	}
}

// ── M7 工具调用 & 沙箱埋点 ─────────────────────────────────────────────────

// RecordToolCall 记录单次工具调用。
// toolName: 原始工具名（由 toolCategory() 映射为受控类别）
// status: "success" | "error" | "timeout"
// sandboxTierLabel: "inprocess" | "l2" | "l3"（调用方根据 SandboxTier 常量映射）
// latencyMs: 工具执行端到端耗时（ms）
//
// 调用方: pkg/action/sandbox_impl.go InProcessSandbox.Run()
func RecordToolCall(ctx context.Context, toolName, status, sandboxTierLabel string, latencyMs float64) {
	cat := toolCategory(toolName)
	if instrToolCallsTotal != nil {
		instrToolCallsTotal.Add(ctx, 1,
			metric.WithAttributes(
				attrCategory(cat),
				attrStatus(status),
				attrSandboxTier(sandboxTierLabel),
			))
	}
	if instrToolLatencyMs != nil {
		instrToolLatencyMs.Record(ctx, latencyMs,
			metric.WithAttributes(attrCategory(cat)))
	}
}

// RecordSandboxExecution 记录沙箱执行次数（独立于 RecordToolCall，用于 sandbox_impl 多入口）。
// tierLabel: "inprocess" | "l2" | "l3"
func RecordSandboxExecution(ctx context.Context, tierLabel string) {
	if instrSandboxTotal != nil {
		instrSandboxTotal.Add(ctx, 1,
			metric.WithAttributes(attrSandboxTier(tierLabel)))
	}
}

// ── M4 任务终态埋点 ─────────────────────────────────────────────────────────

// RecordTaskOutcome 记录任务终态（S_COMPLETE / S_FAILED）。
// 驱动 polaris_task_success_rate ObservableGauge。
// 调用方: pkg/cognition/kernel/agent.go Run() 终态检查处。
func RecordTaskOutcome(_ context.Context, success bool) {
	taskTotalCount.Add(1)
	if success {
		taskSuccessCount.Add(1)
	}
}

// ── 运行态控制 ─────────────────────────────────────────────────────────────

// SetActiveAgents 设置当前活跃 Agent 数量（驱动 polaris_agents_active Gauge）。
// 调用方: pkg/swarm/orchestrator 或 M8 调度层，在 Agent 启动/终止时调用。
func SetActiveAgents(count int) {
	activeAgentsCount.Store(int64(count))
}

// SandboxTierLabel 将 protocol.SandboxTier 常量转为受控 label 字符串。
// 供 sandbox_impl.go 调用，避免在调用方引入字符串魔法值。
func SandboxTierLabel(tier int) string {
	switch tier {
	case 1: // protocol.SandboxInProcess
		return "inprocess"
	case 2: // protocol.SandboxL2
		return "l2"
	case 3: // protocol.SandboxL3
		return "l3"
	default:
		return fmt.Sprintf("tier%d", tier)
	}
}
