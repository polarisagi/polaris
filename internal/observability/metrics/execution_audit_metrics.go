package metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RecordExecutionAuditRecord 使用有限标签累计物理执行审计记录尝试数。
func RecordExecutionAuditRecord(ctx context.Context, phase, status string) {
	if phase != "intent" && phase != "completion" {
		phase = "other"
	}
	if status != "success" && status != "failure" {
		status = "other"
	}
	if InstrExecutionAuditRecordsTotal != nil {
		InstrExecutionAuditRecordsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("phase", phase),
			attribute.String("status", status),
		))
	}
}
