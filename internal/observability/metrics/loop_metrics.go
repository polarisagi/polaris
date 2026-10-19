package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Agentic Loop 可观测性指标初始化与记录函数（ADR-0115 / WP2）。
func initLoopInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error

	InstrAgentLoopRoundsTotal, err = meter.Int64Counter(
		"polaris.agent.loop_rounds_total",
		metric.WithDescription("Agentic loop total rounds executed"),
	)
	ie.capture("polaris.agent.loop_rounds_total", err)

	InstrAgentLoopTurnDurationSeconds, err = meter.Float64Histogram(
		"polaris.agent.loop_turn_duration_seconds",
		metric.WithDescription("Agentic loop turn wall clock duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300),
	)
	ie.capture("polaris.agent.loop_turn_duration_seconds", err)

	InstrAgentLoopTerminationTotal, err = meter.Int64Counter(
		"polaris.agent.loop_termination_total",
		metric.WithDescription("Agentic loop termination reason distribution (reason=auto|finish|budget|guard|interrupt)"),
	)
	ie.capture("polaris.agent.loop_termination_total", err)

	InstrAgentObservationMaskedTotal, err = meter.Int64Counter(
		"polaris.agent.observation_masked_total",
		metric.WithDescription("Total tool observations masked with read_tool_ref"),
	)
	ie.capture("polaris.agent.observation_masked_total", err)

	InstrAgentLoopToolParallelTotal, err = meter.Int64Counter(
		"polaris.agent.loop_tool_parallel_total",
		metric.WithDescription("Agentic loop tool executions by execution mode (mode=parallel|serial)"),
	)
	ie.capture("polaris.agent.loop_tool_parallel_total", err)
}

// RecordLoopRound 记录一次 loop round。
func RecordLoopRound(ctx context.Context) {
	if InstrAgentLoopRoundsTotal != nil {
		InstrAgentLoopRoundsTotal.Add(ctx, 1)
	}
}

// RecordLoopDuration 记录 loop 回合执行时间（秒）。
func RecordLoopDuration(ctx context.Context, seconds float64) {
	if InstrAgentLoopTurnDurationSeconds != nil {
		InstrAgentLoopTurnDurationSeconds.Record(ctx, seconds)
	}
}

// RecordLoopTermination 记录 loop 终止原因。
func RecordLoopTermination(ctx context.Context, reason string) {
	if InstrAgentLoopTerminationTotal != nil {
		InstrAgentLoopTerminationTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("reason", reason),
		))
	}
}

// RecordObservationMasked 记录遮蔽的工具观察数。
func RecordObservationMasked(ctx context.Context, count int64) {
	if InstrAgentObservationMaskedTotal != nil && count > 0 {
		InstrAgentObservationMaskedTotal.Add(ctx, count)
	}
}

// RecordLoopToolParallel 记录工具并发与串行执行指标（R2-06）。
func RecordLoopToolParallel(ctx context.Context, mode string, count int64) {
	if InstrAgentLoopToolParallelTotal != nil && count > 0 {
		InstrAgentLoopToolParallelTotal.Add(ctx, count, metric.WithAttributes(
			attribute.String("mode", mode),
		))
	}
}
