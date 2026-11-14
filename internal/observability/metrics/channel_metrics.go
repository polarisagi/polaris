package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func initChannelInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error
	InstrChannelEventsReceivedTotal, err = meter.Int64Counter(
		"polaris.extension.channel.events_received_total",
		metric.WithDescription("插件 channel 收到的消息事件数"),
	)
	ie.capture("polaris.extension.channel.events_received_total", err)
	InstrChannelEventsRejectedTotal, err = meter.Int64Counter(
		"polaris.extension.channel.events_rejected_total",
		metric.WithDescription("插件 channel 拒绝的消息事件数 (label: reason)"),
	)
	ie.capture("polaris.extension.channel.events_rejected_total", err)
	InstrChannelTurnsTotal, err = meter.Int64Counter(
		"polaris.extension.channel.turns_total",
		metric.WithDescription("插件 channel 会话轮次结果 (label: outcome)"),
	)
	ie.capture("polaris.extension.channel.turns_total", err)
}

func RecordChannelEventReceived(ctx context.Context) {
	if InstrChannelEventsReceivedTotal != nil {
		InstrChannelEventsReceivedTotal.Add(ctx, 1)
	}
}

func RecordChannelEventRejected(ctx context.Context, reason string) {
	if InstrChannelEventsRejectedTotal == nil {
		return
	}
	switch reason {
	case "malformed", "route_invalid", "runner_unavailable", "binding_inactive", "duplicate":
	default:
		reason = "other"
	}
	InstrChannelEventsRejectedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func RecordChannelTurn(ctx context.Context, outcome string) {
	if InstrChannelTurnsTotal == nil {
		return
	}
	if outcome != "success" && outcome != "failure" {
		outcome = "other"
	}
	InstrChannelTurnsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
