package dispatch

import "context"

// AuditLogger dispatch 包对审计追踪器的消费端接口。
// 实现：security.AuditTrail（RecordAudit 方法）
type AuditLogger interface {
	// RecordAudit 记录工具执行的审计事件（工具名 + 原始参数）。
	RecordAudit(ctx context.Context, toolName string, payload []byte) error
}
