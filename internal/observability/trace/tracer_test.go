package trace

import (
	"context"
	"testing"
	"time"
)

// TestSetDefaultExporters_NewTracerInherits 验证 boot 期 SetDefaultExporters 注册的导出器
// 会被之后所有 NewTracer() 构造的新实例自动继承（ADR-0069：internal/knowledge/rag_impl.go、
// retriever.go 等调用点各自独立构造 Tracer，若不做这层自动继承，boot 期注册的导出器永远
// 不会被这些分散的 Tracer 实例使用）。
func TestSetDefaultExporters_NewTracerInherits(t *testing.T) {
	defer SetDefaultExporters(nil) // 清理全局状态，避免污染其他测试

	mockExp := &mockExporter{}
	SetDefaultExporters([]SpanExporter{mockExp})

	tracer := NewTracer()
	span, _ := tracer.StartSpan(context.Background(), SpanMemoryOp, "test")
	tracer.EndSpan(span)

	time.Sleep(20 * time.Millisecond)

	if got := mockExp.exported.Load(); got != 1 {
		t.Errorf("expected default exporter to receive 1 span, got %d", got)
	}
}

// TestSetDefaultExporters_NilIsNoop 验证未调用 SetDefaultExporters（或显式传 nil）时，
// NewTracer() 构造的 Tracer 无任何导出器——等价于 ADR-0069 决策点 4 所称的默认
// NoopExporter 行为，不需要单独定义 NoopExporter 类型。
func TestSetDefaultExporters_NilIsNoop(t *testing.T) {
	defer SetDefaultExporters(nil)
	SetDefaultExporters(nil)

	tracer := NewTracer()
	if len(tracer.exporters) != 0 {
		t.Errorf("expected no default exporters, got %d", len(tracer.exporters))
	}
}
