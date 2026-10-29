package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// 模型路由可观测性指标初始化与记录函数（ADR-0115 / WP3）。

func initModelRouteInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error

	InstrAgentModelRouteTotal, err = meter.Int64Counter(
		"polaris.agent.model_route_total",
		metric.WithDescription("Agent model routing source and fallback distribution"),
	)
	ie.capture("polaris.agent.model_route_total", err)

	InstrLLMModelRefFallbackTotal, err = meter.Int64Counter(
		"polaris.llm.model_ref_fallback_total",
		metric.WithDescription("LLM router fallback from model reference to pool"),
	)
	ie.capture("polaris.llm.model_ref_fallback_total", err)
}

// RecordAgentModelRoute 记录 Agent 模型路由决策分布。
func RecordAgentModelRoute(ctx context.Context, source, fallback string) {
	if InstrAgentModelRouteTotal != nil {
		InstrAgentModelRouteTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", source),
			attribute.String("fallback", fallback),
		))
	}
}

// RecordModelRefFallback 记录模型引用回退到池。
func RecordModelRefFallback(ctx context.Context) {
	if InstrLLMModelRefFallbackTotal != nil {
		InstrLLMModelRefFallbackTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("route", "model_ref_fallback"),
		))
	}
}
