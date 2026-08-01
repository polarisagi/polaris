package curriculum

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// gapFillMockProvider 返回一段可解析的合法技能 JSON，供 synthesizeSkill 走完
// generateSchema 阶段直到 registry.Register。
type gapFillMockProvider struct{}

func (m *gapFillMockProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: `{"name":"missing_tool","description":"d","version":"1.0.0","input_schema":{},"instructions":"do it"}`}, nil
}
func (m *gapFillMockProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	return nil, nil
}
func (m *gapFillMockProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *gapFillMockProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *gapFillMockProvider) ModelID() string                      { return "mock" }
func (m *gapFillMockProvider) MaxConcurrency() int                  { return 1 }
func (m *gapFillMockProvider) SupportsModel(model string) bool      { return true }
func (m *gapFillMockProvider) ID() string                           { return "mock" }
func (m *gapFillMockProvider) Close() error                         { return nil }

// alwaysFailToolRegistry 的 Register 总是失败，用于验证阶段02修复（GR-7-002）：
// 合成技能注册失败必须向上返回错误，不得像修复前 `_ = w.registry.Register(skill)`
// 那样静默吞没（LLM 调用成本已花掉但成果彻底丢失且不可观测）。
type alwaysFailToolRegistry struct{}

func (r *alwaysFailToolRegistry) Register(tool types.Tool) error {
	return apperr.New(apperr.CodeInternal, "simulated registry write failure")
}
func (r *alwaysFailToolRegistry) Lookup(name string) (types.Tool, error) {
	return types.Tool{}, apperr.New(apperr.CodeNotFound, "not found")
}
func (r *alwaysFailToolRegistry) List() []types.Tool { return nil }
func (r *alwaysFailToolRegistry) ExecuteTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return nil, apperr.New(apperr.CodeNotFound, "not implemented")
}

func TestGapFillWorker_SynthesizeSkill_RegisterFailure_ReturnsError_S02(t *testing.T) {
	w := NewGapFillWorker(nil, &gapFillMockProvider{}, &alwaysFailToolRegistry{})

	err := w.synthesizeSkill(context.Background(), "missing_tool")
	if err == nil {
		t.Fatal("expected error when registry.Register fails, got nil")
	}
}

// TestGapFillWorker_HandleOutbox_RegisterFailure_PropagatesToCaller_S02 验证
// synthesizeSkill 的错误会一路传导至 HandleOutbox 的返回值（供 outbox 重试
// 机制接手重新触发合成），而不是被中途某一层吸收。
func TestGapFillWorker_HandleOutbox_RegisterFailure_PropagatesToCaller_S02(t *testing.T) {
	db := newMemDB(t)
	if _, err := db.Exec(`
		CREATE TABLE capability_gap_log (
			id TEXT PRIMARY KEY, session_id TEXT, task_id TEXT, required_tool TEXT,
			description TEXT, status TEXT, trust_tier INTEGER, created_at INTEGER, updated_at INTEGER
		)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	w := NewGapFillWorker(db, &gapFillMockProvider{}, &alwaysFailToolRegistry{})

	record := &store.OutboxRecord{Payload: []byte(`{"error":"tool not found: missing_tool"}`)}
	err := w.HandleOutbox(context.Background(), record)
	if err == nil {
		t.Fatal("expected HandleOutbox to propagate registry.Register failure, got nil")
	}
}
