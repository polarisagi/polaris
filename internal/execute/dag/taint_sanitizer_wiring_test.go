package dag

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockSchemaToolExecutor 按 name 返回预注册的 types.Tool（供 InputSchema 严格性测试）。
type mockSchemaToolExecutor struct {
	tools map[string]types.Tool
}

func (m *mockSchemaToolExecutor) Lookup(name string) (types.Tool, error) {
	t, ok := m.tools[name]
	if !ok {
		return types.Tool{}, apperrNotFound(name)
	}
	return t, nil
}

func (m *mockSchemaToolExecutor) ExecuteWithTaint(_ context.Context, _ string, _ []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true}, nil
}

func apperrNotFound(name string) error {
	return &notFoundErr{name: name}
}

type notFoundErr struct{ name string }

func (e *notFoundErr) Error() string { return "tool not found: " + e.name }

// mockReviewChecker 模拟 ExemptionVault.IsReviewed：agentID+content 精确匹配时通过。
type mockReviewChecker struct {
	agentID string
	content []byte
}

func (m *mockReviewChecker) IsReviewed(agentID string, content []byte) bool {
	return agentID == m.agentID && string(content) == string(m.content)
}

// ─── hasStrictSchema / schemaNodeIsStrict ───────────────────────────────────

func TestHasStrictSchema_BareStringRejected(t *testing.T) {
	tool := types.Tool{InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
	}}
	if hasStrictSchema(tool) {
		t.Error("bare {type:string} with no format/pattern/enum/const must not be strict")
	}
}

func TestHasStrictSchema_EnumConstrainedStringAccepted(t *testing.T) {
	tool := types.Tool{InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
		},
	}}
	if !hasStrictSchema(tool) {
		t.Error("string field with enum constraint should be considered strict")
	}
}

func TestHasStrictSchema_NestedBareStringRejectsWholeStruct(t *testing.T) {
	tool := types.Tool{InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"outer": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"inner": map[string]any{"type": "string"}, // 裸 string，无约束
				},
			},
		},
	}}
	if hasStrictSchema(tool) {
		t.Error("any unconstrained nested leaf string must reject the whole structure")
	}
}

func TestHasStrictSchema_NoSchemaRejected(t *testing.T) {
	if hasStrictSchema(types.Tool{}) {
		t.Error("tool with nil InputSchema must not be considered strict")
	}
}

func TestHasStrictSchema_JSONRawMessageNormalized(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","format":"uuid"}}}`)
	tool := types.Tool{InputSchema: raw}
	if !hasStrictSchema(tool) {
		t.Error("json.RawMessage InputSchema with format constraint should normalize and pass")
	}
}

func TestHasStrictSchema_FreeObjectRejected(t *testing.T) {
	tool := types.Tool{InputSchema: map[string]any{
		"type":                 "object",
		"additionalProperties": true,
	}}
	if hasStrictSchema(tool) {
		t.Error("free-form object with no properties must be rejected (fail-closed)")
	}
}

// ─── attemptSchemaDowngrade ──────────────────────────────────────────────────

func TestAttemptSchemaDowngrade_StrictSchemaDowngrades(t *testing.T) {
	vCtx := &DAGValidationContext{
		ToolExecutor: &mockSchemaToolExecutor{tools: map[string]types.Tool{
			"strict_tool": {Name: "strict_tool", InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
				},
			}},
		}},
	}
	node := protocol.ExecNode{ID: "n1", ToolName: "strict_tool", Args: []byte(`{"mode":"a"}`)}
	got := attemptSchemaDowngrade(vCtx, node, types.TaintHigh)
	if got != types.TaintMedium {
		t.Errorf("expected downgrade to TaintMedium (hard cap), got %v", got)
	}
}

func TestAttemptSchemaDowngrade_LooseSchemaNoDowngrade(t *testing.T) {
	vCtx := &DAGValidationContext{
		ToolExecutor: &mockSchemaToolExecutor{tools: map[string]types.Tool{
			"loose_tool": {Name: "loose_tool", InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
			}},
		}},
	}
	node := protocol.ExecNode{ID: "n1", ToolName: "loose_tool", Args: []byte(`{"query":"anything"}`)}
	got := attemptSchemaDowngrade(vCtx, node, types.TaintHigh)
	if got != types.TaintHigh {
		t.Errorf("expected no downgrade (level unchanged), got %v", got)
	}
}

func TestAttemptSchemaDowngrade_UnknownToolNoDowngrade(t *testing.T) {
	vCtx := &DAGValidationContext{ToolExecutor: &mockSchemaToolExecutor{tools: map[string]types.Tool{}}}
	node := protocol.ExecNode{ID: "n1", ToolName: "ghost_tool", Args: []byte(`{}`)}
	got := attemptSchemaDowngrade(vCtx, node, types.TaintHigh)
	if got != types.TaintHigh {
		t.Errorf("unregistered tool should not downgrade, got %v", got)
	}
}

// ─── attemptUserReviewDowngrade ──────────────────────────────────────────────

func TestAttemptUserReviewDowngrade_ValidReviewGrantsUserReviewed(t *testing.T) {
	args := []byte(`{"content":"reviewed-content"}`)
	vCtx := &DAGValidationContext{
		AgentID:       "agent-1",
		ReviewChecker: &mockReviewChecker{agentID: "agent-1", content: args},
	}
	node := protocol.ExecNode{ID: "n1", ToolName: "write_file", Args: args}
	got := attemptUserReviewDowngrade(vCtx, node, types.TaintHigh)
	if got != types.TaintUserReviewed {
		t.Errorf("expected TaintUserReviewed, got %v", got)
	}
}

func TestAttemptUserReviewDowngrade_NoCheckerNoChange(t *testing.T) {
	node := protocol.ExecNode{ID: "n1", ToolName: "write_file", Args: []byte(`{}`)}
	got := attemptUserReviewDowngrade(&DAGValidationContext{}, node, types.TaintHigh)
	if got != types.TaintHigh {
		t.Errorf("nil ReviewChecker must not change level, got %v", got)
	}
}

func TestAttemptUserReviewDowngrade_MismatchedContentNoChange(t *testing.T) {
	vCtx := &DAGValidationContext{
		AgentID:       "agent-1",
		ReviewChecker: &mockReviewChecker{agentID: "agent-1", content: []byte("approved-content")},
	}
	node := protocol.ExecNode{ID: "n1", ToolName: "write_file", Args: []byte("different-content")}
	got := attemptUserReviewDowngrade(vCtx, node, types.TaintHigh)
	if got != types.TaintHigh {
		t.Errorf("mismatched content hash must not downgrade, got %v", got)
	}
}

// ─── 端到端 ValidateDAG：降级路径最终放行 write_network/非只读工具 ────────────

func TestValidateDAG_SchemaDowngrade_AllowsWriteNetworkAtMedium(t *testing.T) {
	toolExec := &mockSchemaToolExecutor{tools: map[string]types.Tool{
		"post_webhook": {
			Name:       "post_webhook",
			Capability: types.CapWriteNetwork,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"event": map[string]any{"type": "string", "enum": []any{"created", "updated"}},
				},
			},
		},
	}}
	plan := &DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "post_webhook", Args: []byte(`{"event":"created"}`)}}}
	vCtx := &DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: types.TaintMedium,
		PolicyGate:       allowAllGateForDagTest{},
		ToolExecutor:     toolExec,
		AgentID:          "agent-x",
	}
	if err := ValidateDAG(context.Background(), vCtx); err != nil {
		t.Errorf("strict-schema TaintMedium write_network call should be allowed after SanitizeBySchema downgrade: %v", err)
	}
}

func TestValidateDAG_UserReviewDowngrade_AllowsHighTaintNonReadOnlyTool(t *testing.T) {
	args := []byte(`{"content":"free text from user"}`)
	toolExec := &mockSchemaToolExecutor{tools: map[string]types.Tool{
		"write_file": {Name: "write_file", Capability: types.CapWriteLocal},
	}}
	plan := &DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "write_file", Args: args}}}
	vCtx := &DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: types.TaintHigh,
		PolicyGate:       allowAllGateForDagTest{},
		ToolExecutor:     toolExec,
		AgentID:          "agent-x",
		ReviewChecker:    &mockReviewChecker{agentID: "agent-x", content: args},
	}
	if err := ValidateDAG(context.Background(), vCtx); err != nil {
		t.Errorf("HITL-reviewed TaintHigh args should be allowed via SanitizeByUserReview: %v", err)
	}
}

func TestValidateDAG_NoDowngrade_StillBlocksAsBeforeBackwardCompat(t *testing.T) {
	// 无 schema、无 review checker：行为应与降级功能引入前完全一致（回归防护）。
	plan := &DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "write_file", Args: []byte(`{"content":"raw"}`)}}}
	vCtx := &DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: types.TaintHigh,
		PolicyGate:       allowAllGateForDagTest{},
		AgentID:          "agent-x",
	}
	err := ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Fatal("without schema/review downgrade, TaintHigh write_file must still be blocked")
	}
}

// allowAllGateForDagTest 供本文件测试使用的最小 PolicyGate 放行实现。
type allowAllGateForDagTest struct{}

func (allowAllGateForDagTest) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return true, nil
}

func (allowAllGateForDagTest) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true, Reason: "allow-all"}, nil
}
