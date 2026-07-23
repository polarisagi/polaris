package trace

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// SpanKind mirrors the gen_ai.* semantic convention.
type SpanKind string

const (
	SpanLLMCall    SpanKind = "gen_ai.llm_call"
	SpanToolCall   SpanKind = "gen_ai.tool_call"
	SpanMemoryOp   SpanKind = "gen_ai.memory_op"
	SpanStateTrans SpanKind = "gen_ai.state_transition"
)

// Span records a single operation within an agent trace.
type Span struct {
	TraceID   string         `json:"trace_id"`
	SpanID    string         `json:"span_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Kind      SpanKind       `json:"kind"`
	Name      string         `json:"name"`
	StartTime time.Time      `json:"start_time"`
	EndTime   time.Time      `json:"end_time,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

// Tracer is the minimal tracing abstraction for agent operations.
type Tracer struct {
	logger    *slog.Logger
	exporters []SpanExporter
}

// defaultExporters 是 boot 期注册的全局默认 SpanExporter 列表（ADR-0069）。
// 调用方（internal/knowledge/rag_impl.go、retriever.go 等）各自用 NewTracer()
// 构造独立 Tracer 实例、互不共享状态；若要求这些分散实例都能导出到外部平台，
// 唯一低侵入方案是让 NewTracer() 自动附加 boot 期注册的默认导出器列表，而非把
// 这些调用点全部改造成注入共享 Tracer 单例（那将是远超"接入导出器"范围的架构
// 改动）。atomic.Pointer 零值 nil = 未配置（默认关闭，等价于 ADR-0069 决策点 4
// 所称的 NoopExporter——Tracer.exporters 为空 slice 时 EndSpan 循环体不执行，
// 语义上与显式 Noop 完全等价，未单独定义 NoopExporter 类型以避免冗余抽象）。
// boot 期只应调用 SetDefaultExporters 一次，运行期只读，符合 Test_inv_NoGlobalVar
// 豁免类别（atomic.* 零值单例）。
//
//nolint:gochecknoglobals // atomic.Pointer 零值单例，boot 期单次写入，运行期只读
var defaultExporters atomic.Pointer[[]SpanExporter]

// SetDefaultExporters 由 boot 期（cmd/polaris/boot_substrate.go）读取
// config.Thresholds.M3Observability.TraceExport 后调用一次，注册全局默认导出器。
// 之后所有 NewTracer() 构造的新 Tracer 实例自动继承该列表。
func SetDefaultExporters(exporters []SpanExporter) {
	defaultExporters.Store(&exporters)
}

func NewTracer() *Tracer {
	t := &Tracer{
		logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})),
	}
	if p := defaultExporters.Load(); p != nil {
		t.exporters = append(t.exporters, *p...)
	}
	return t
}

func (t *Tracer) RegisterExporter(e SpanExporter) {
	t.exporters = append(t.exporters, e)
}

// StartSpan starts a new span. 若 ctx 中已有父 Span，则传播 TraceID 并设置 ParentID，
// 保证同一请求链路内所有 Span 共享同一 TraceID，形成完整 trace 树。
func (t *Tracer) StartSpan(ctx context.Context, kind SpanKind, name string) (*Span, context.Context) {
	span := &Span{
		TraceID:   newID(),
		SpanID:    newID(),
		Kind:      kind,
		Name:      name,
		StartTime: time.Now(),
	}
	// 传播父 Span 的 TraceID 和 SpanID，使 trace 树可关联。
	if parent := SpanFromContext(ctx); parent != nil {
		span.TraceID = parent.TraceID
		span.ParentID = parent.SpanID
	}
	t.logger.Info("span_start",
		"trace_id", span.TraceID,
		"span_id", span.SpanID,
		"parent_id", span.ParentID,
		"kind", string(kind),
		"name", name,
	)
	return span, context.WithValue(ctx, ctxKey{name: "observability_span"}, span)
}

func (t *Tracer) EndSpan(span *Span) {
	span.EndTime = time.Now()
	t.logger.Info("span_end",
		"trace_id", span.TraceID,
		"span_id", span.SpanID,
		"duration_ms", span.EndTime.Sub(span.StartTime).Milliseconds(),
	)
	for _, e := range t.exporters {
		// ADR-0069 R2：导出必须异步、尽力而为，绝不阻塞调用方热路径；且不得因导出器
		// 内部 panic 拖垮整个进程——此前版本用裸 go 语句 + //custom-nolint 逃逸豁免，
		// 理由是"trace export 不需要 SafeGo 管理"，但该理由站不住脚：SafeGo 的核心价值
		// 是 panic 恢复而非"重要性分级"，一个默认关闭的可选可观测性导出器一旦 panic
		// 反而会拖垮本应无关的主进程，属于本末倒置，复核改为 concurrent.SafeGo。
		exporter := e
		s := span
		concurrent.SafeGo(context.Background(), "trace_exporter.export_span", func(_ context.Context) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exporter.ExportSpan(ctx, s); err != nil {
				t.logger.Warn("failed to export span", "err", err, "trace_id", s.TraceID)
				metrics.GlobalTraceExporterErrorsTotal.Add(1)
			}
		})
	}
}

type ctxKey struct{ name string }

func SpanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(ctxKey{name: "observability_span"}).(*Span)
	return s
}

// DetachedWithLink creates a new context but carries over the trace span from the parent context.
func DetachedWithLink(parent context.Context) context.Context {
	span := SpanFromContext(parent)
	ctx := context.Background()
	if span != nil {
		return context.WithValue(ctx, ctxKey{name: "observability_span"}, span)
	}
	return ctx
}

// spanIDCounter 防止同纳秒内生成重复 SpanID/TraceID。
var spanIDCounter atomic.Int64

// newID 生成 32 字符十六进制 ID：高 16 位来自时间戳，低 16 位来自单调递增计数器。
// 避免了纯时间戳方案在同纳秒并发调用时的碰撞。
func newID() string {
	hi := time.Now().UnixNano()
	lo := spanIDCounter.Add(1)
	return fmtHex2(hi, lo)
}

func fmtHex2(hi, lo int64) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 32)
	for i := 15; i >= 0; i-- {
		b[i] = hex[hi&0xf]
		hi >>= 4
	}
	for i := 31; i >= 16; i-- {
		b[i] = hex[lo&0xf]
		lo >>= 4
	}
	return string(b)
}
