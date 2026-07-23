package trace

import "context"

// SpanExporter 导出已结束的 Span 到外部可观测平台。实现须并发安全、非阻塞友好。
type SpanExporter interface {
	ExportSpan(ctx context.Context, s *Span) error
	Shutdown(ctx context.Context) error
}
