package agent

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

// lowPrefixHitRatio 低于此命中率且没有已知原因的非首调，归因为 unknown（提示有未被登记的前缀断裂源）。
const lowPrefixHitRatio = 0.5

// observePrefixCacheLocked 记录本次内核调用的前缀缓存命中率与断裂归因（ADR-0120 决策八）。
// 命中率进指标直方图，prefix_break 与命中率进 span 属性与 debug 日志。调用方持有 a.sCtx.Mu。
func (a *Agent) observePrefixCacheLocked(ctx context.Context, purpose string, inputTokens, cacheHitTokens int) {
	reason := a.sCtx.TakePrefixBreakLocked()
	if inputTokens <= 0 {
		return
	}
	callIndex := a.sCtx.LoopRound + 1
	ratio := float64(cacheHitTokens) / float64(inputTokens)
	if reason == "" && callIndex > 1 && ratio < lowPrefixHitRatio {
		reason = "unknown"
	}
	metrics.RecordLLMPrefixCacheHitRatio(ctx, purpose, callIndex, inputTokens, cacheHitTokens)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("polaris.prefix_break", reason),
		attribute.Float64("polaris.cache_hit_ratio", ratio),
	)
	slog.DebugContext(ctx, "llm: prefix cache", "purpose", purpose, "call_index", callIndex,
		"input_tokens", inputTokens, "cache_hit_tokens", cacheHitTokens,
		"cache_hit_ratio", ratio, "prefix_break", reason)
}
