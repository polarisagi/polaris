package observability

import (
	"context"
	"log/slog"
	"os"
	"time"
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
	logger *slog.Logger
}

func NewTracer() *Tracer {
	return &Tracer{
		logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})),
	}
}

func (t *Tracer) StartSpan(ctx context.Context, kind SpanKind, name string) (*Span, context.Context) {
	span := &Span{
		TraceID:   newID(),
		SpanID:    newID(),
		Kind:      kind,
		Name:      name,
		StartTime: time.Now(),
	}
	t.logger.Info("span_start",
		"trace_id", span.TraceID,
		"span_id", span.SpanID,
		"kind", string(kind),
		"name", name,
	)
	return span, context.WithValue(ctx, ctxKeySpan, span)
}

func (t *Tracer) EndSpan(span *Span) {
	span.EndTime = time.Now()
	t.logger.Info("span_end",
		"trace_id", span.TraceID,
		"span_id", span.SpanID,
		"duration_ms", span.EndTime.Sub(span.StartTime).Milliseconds(),
	)
}

type ctxKey struct{ name string }

var ctxKeySpan = ctxKey{name: "observability_span"}

func SpanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(ctxKeySpan).(*Span)
	return s
}

func newID() string {
	return fmtHex(time.Now().UnixNano())
}

func fmtHex(n int64) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = hex[n&0xf]
		n >>= 4
	}
	return string(b)
}
