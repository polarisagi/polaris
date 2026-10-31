package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ExecutionAuditGate 在允许物理动作前持久化意图，完成后持久化结果元数据；不会重跑动作。
type ExecutionAuditGate struct {
	mu       sync.Mutex
	logger   protocol.AuditLogger
	inflight map[string]inflightExecutionAudit
	pending  []pendingExecutionCompletion
}

type inflightExecutionAudit struct {
	meta   map[string]any
	span   *trace.Span
	tracer *trace.Tracer
}

type pendingExecutionCompletion struct {
	id   string
	meta map[string]any
}

func NewExecutionAuditGate(logger protocol.AuditLogger) *ExecutionAuditGate {
	return &ExecutionAuditGate{logger: logger, inflight: make(map[string]inflightExecutionAudit)}
}

func (g *ExecutionAuditGate) BeginExecution(ctx context.Context, intent protocol.ExecutionAuditIntent) (string, error) {
	if g == nil || g.logger == nil {
		return "", apperr.New(apperr.CodeForbidden, "execution audit unavailable; refusing physical action")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.flushPendingLocked(ctx); err != nil {
		return "", apperr.Wrap(apperr.CodeStorageUnavailable, "execution audit recovery failed; refusing new physical action", err)
	}
	id, err := newExecutionAuditID()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "execution audit ID generation failed", err)
	}
	base := executionAuditBase(ctx, id, intent)
	meta := copyAuditMeta(base)
	meta["phase"] = "intent"
	meta["status"] = "pending"
	meta["error_code"] = ""
	tracer := trace.NewTracer()
	span, spanCtx := tracer.StartSpan(ctx, trace.SpanToolCall, "security.execution")
	span.Attrs = map[string]any{"execution_id": id, "tool": intent.Tool, "source": intent.Source}
	if err := g.logger.Log(ctx, "execution_audit", meta); err != nil {
		span.Attrs["status"] = "failure"
		span.Attrs["phase"] = "intent"
		tracer.EndSpan(span)
		metrics.RecordExecutionAuditRecord(spanCtx, "intent", "failure")
		return "", apperr.Wrap(apperr.CodeStorageUnavailable, "execution intent audit failed; refusing physical action", err)
	}
	span.Attrs["status"] = "in_progress"
	g.inflight[id] = inflightExecutionAudit{meta: base, span: span, tracer: tracer}
	metrics.RecordExecutionAuditRecord(spanCtx, "intent", "success")
	return id, nil
}

func (g *ExecutionAuditGate) CompleteExecution(ctx context.Context, id string, success bool, errorCode string) error {
	if g == nil || g.logger == nil {
		return apperr.New(apperr.CodeStorageUnavailable, "execution audit unavailable during completion")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	inflight, ok := g.inflight[id]
	if !ok {
		return apperr.New(apperr.CodeNotFound, "execution audit completion has no matching intent")
	}
	meta := copyAuditMeta(inflight.meta)
	meta["phase"] = "completion"
	meta["status"] = "success"
	if !success {
		meta["status"] = "failure"
	}
	meta["error_code"] = errorCode
	if err := g.logger.Log(ctx, "execution_audit", meta); err != nil {
		// 物理动作已有真实结果；仅保留安全的完成元数据，供后续 BeginExecution 补记。
		g.pending = append(g.pending, pendingExecutionCompletion{id: id, meta: meta})
		delete(g.inflight, id)
		inflight.span.Attrs["status"] = "completion_audit_failure"
		inflight.span.Attrs["error_code"] = errorCode
		inflight.tracer.EndSpan(inflight.span)
		metrics.RecordExecutionAuditRecord(ctx, "completion", "failure")
		return apperr.Wrap(apperr.CodeStorageUnavailable, "execution completion audit failed; physical result preserved", err)
	}
	delete(g.inflight, id)
	inflight.span.Attrs["status"] = stringStatus(success)
	inflight.span.Attrs["error_code"] = errorCode
	inflight.tracer.EndSpan(inflight.span)
	metrics.RecordExecutionAuditRecord(ctx, "completion", "success")
	return nil
}

func (g *ExecutionAuditGate) flushPendingLocked(ctx context.Context) error {
	for len(g.pending) > 0 {
		pending := g.pending[0]
		if err := g.logger.Log(ctx, "execution_audit", copyAuditMeta(pending.meta)); err != nil {
			metrics.RecordExecutionAuditRecord(ctx, "completion", "failure")
			return apperr.Wrap(apperr.CodeStorageUnavailable, "pending execution audit flush failed", err)
		}
		metrics.RecordExecutionAuditRecord(ctx, "completion", "success")
		g.pending[0] = pendingExecutionCompletion{}
		g.pending = g.pending[1:]
	}
	return nil
}

func stringStatus(success bool) string {
	if success {
		return "success"
	}
	return "failure"
}

func executionAuditBase(ctx context.Context, id string, intent protocol.ExecutionAuditIntent) map[string]any {
	digest := sha256.Sum256(intent.Args)
	meta := map[string]any{
		"execution_id": id,
		"tool":         intent.Tool,
		"source":       intent.Source,
		"args_length":  len(intent.Args),
		"args_sha256":  hex.EncodeToString(digest[:]),
	}
	if ctx != nil {
		if agentID, ok := ctx.Value(protocol.CtxAgentIDKey{}).(string); ok && agentID != "" {
			meta["agent_id"] = agentID
		}
		if sessionID, ok := ctx.Value(protocol.CtxSessionIDKey{}).(string); ok && sessionID != "" {
			meta["session_id"] = sessionID
		}
		if taskID, ok := ctx.Value(protocol.CtxTaskIDKey{}).(string); ok && taskID != "" {
			meta["task_id"] = taskID
		}
	}
	return meta
}

func copyAuditMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		out[key] = value
	}
	return out
}

func newExecutionAuditID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "execution audit random ID", err)
	}
	return "exec_" + hex.EncodeToString(raw[:]), nil
}
