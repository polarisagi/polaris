package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	// InstrHITLAutoApprovedTotal 记录 full 档位下 HITL 自动放行事件计数（V9 / HE-1）。
	InstrHITLAutoApprovedTotal metric.Int64Counter
)

func initHITLAutoApprovedInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error
	InstrHITLAutoApprovedTotal, err = meter.Int64Counter(
		"polaris.security.hitl_auto_approved_total",
		metric.WithDescription("HITL 自动放行次数 (labels: tool, checkpoint_type)"),
	)
	ie.capture("polaris.security.hitl_auto_approved_total", err)
}

// RecordHITLAutoApproved 记录一次 full 档下的 HITL 自动放行（HE-1）。
func RecordHITLAutoApproved(ctx context.Context, tool, checkpointType string) {
	if InstrHITLAutoApprovedTotal != nil {
		InstrHITLAutoApprovedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("tool", tool),
			attribute.String("checkpoint_type", checkpointType),
		))
	}
}
