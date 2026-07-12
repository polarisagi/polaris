package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/polarisagi/polaris/internal/agent/fsm"

	"github.com/polarisagi/polaris/internal/execute/dag"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── Mock PolicyGate ─────────────────────────────────────────────────────────

// allowAllGate 放行所有请求。
type allowAllGate struct{}

func (g *allowAllGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return true, nil
}
func (g *allowAllGate) Review(_ context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true, Reason: "allow-all"}, nil
}

// denyAllGate 拒绝所有请求（用于测试 fail-closed）。
type denyAllGate struct{}

func (g *denyAllGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return false, nil
}
func (g *denyAllGate) Review(_ context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: false, Reason: "deny-by-default"}, nil
}

// errorGate 模拟 PolicyGate 评估出错（fail-closed 场景）。
type errorGate struct{}

func (g *errorGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return false, apperr.New(apperr.CodeForbidden, "policy engine error")
}
func (g *errorGate) Review(_ context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{}, apperr.New(apperr.CodeForbidden, "policy engine error")
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

func makeSimplePlan(nodeID, toolName string, args []byte) *dag.DAGPlan {
	return &dag.DAGPlan{
		Nodes: []dag.ExecNode{
			{ID: nodeID, ToolName: toolName, Args: args},
		},
	}
}

// ─── L0 拓扑校验测试 ──────────────────────────────────────────────────────────

func TestValidateDAG_L0_NilPlan(t *testing.T) {
	vCtx := &dag.DAGValidationContext{Plan: nil, PolicyGate: &allowAllGate{}}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("nil plan should be rejected")
	}
	var valErr *dag.DAGValidationError
	if errors.As(err, &valErr) {
		if valErr.Layer != "L0" {
			t.Errorf("expected L0 error, got %s", valErr.Layer)
		}
	}
}

func TestValidateDAG_L0_NodeCountExceeded(t *testing.T) {
	nodes := make([]dag.ExecNode, 51)
	for i := range nodes {
		nodes[i] = dag.ExecNode{ID: string(rune('a' + i%26))}
	}
	plan := &dag.DAGPlan{Nodes: nodes}
	vCtx := &dag.DAGValidationContext{Plan: plan, PolicyGate: &allowAllGate{}}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("node count > 50 should be rejected")
	}
	var valErr *dag.DAGValidationError
	if errors.As(err, &valErr) && valErr.Layer != "L0" {
		t.Errorf("expected L0 error, got %s", valErr.Layer)
	}
}

func TestValidateDAG_L0_CycleDetected(t *testing.T) {
	plan := &dag.DAGPlan{
		Nodes: []dag.ExecNode{
			{ID: "A", DependsOn: []string{"B"}},
			{ID: "B", DependsOn: []string{"A"}},
		},
	}
	vCtx := &dag.DAGValidationContext{Plan: plan, PolicyGate: &allowAllGate{}}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("cycle should be rejected by L0")
	}
	var valErr *dag.DAGValidationError
	if errors.As(err, &valErr) && valErr.Layer != "L0" {
		t.Errorf("expected L0 error, got %s", valErr.Layer)
	}
}

// ─── L1 TaintGate 测试 ────────────────────────────────────────────────────────

func TestValidateDAG_L1Taint_HighTaintWriteToolBlocked(t *testing.T) {
	// TaintHigh + 非只读工具 → 应被 L1_taint 拦截
	plan := makeSimplePlan("node1", "write_file", []byte(`{"content":"user input"}`))
	vCtx := &dag.DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: types.TaintHigh,
		PolicyGate:       &allowAllGate{},
		AgentID:          "test-agent",
	}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("TaintHigh args to write_file should be blocked by L1_taint")
	}
	var valErr *dag.DAGValidationError
	if errors.As(err, &valErr) {
		if valErr.Layer != "L1_taint" {
			t.Errorf("expected L1_taint error, got %s", valErr.Layer)
		}
		if valErr.NodeID != "node1" {
			t.Errorf("expected nodeID=node1, got %s", valErr.NodeID)
		}
	}
}

func TestValidateDAG_L1Taint_HighTaintReadToolAllowed(t *testing.T) {
	// TaintHigh + 只读工具（read_file）→ L1_taint 应放行（只读不写状态）
	plan := makeSimplePlan("node1", "read_file", []byte(`{"path":"/tmp/safe"}`))
	vCtx := &dag.DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: types.TaintHigh,
		PolicyGate:       &allowAllGate{},
		AgentID:          "test-agent",
	}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err != nil {
		t.Errorf("TaintHigh args to read_file should be allowed: %v", err)
	}
}

func TestValidateDAG_L1Taint_MediumTaintAllowed(t *testing.T) {
	// TaintMedium + 任意工具 → L1_taint 不拦截（TaintHigh 才触发）
	plan := makeSimplePlan("node1", "write_file", []byte(`{"content":"medium"}`))
	vCtx := &dag.DAGValidationContext{
		Plan:             plan,
		ActiveTaintLevel: types.TaintMedium,
		PolicyGate:       &allowAllGate{},
		AgentID:          "test-agent",
	}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err != nil {
		t.Errorf("TaintMedium should not trigger L1_taint: %v", err)
	}
}

// ─── L1 PolicyGate 测试 ───────────────────────────────────────────────────────

func TestValidateDAG_L1Policy_AllowAllGate(t *testing.T) {
	plan := makeSimplePlan("node1", "read_file", nil)
	vCtx := &dag.DAGValidationContext{
		Plan:       plan,
		PolicyGate: &allowAllGate{},
		AgentID:    "test-agent",
	}
	if err := dag.ValidateDAG(context.Background(), vCtx); err != nil {
		t.Errorf("allowAllGate should pass: %v", err)
	}
}

func TestValidateDAG_L1Policy_DenyAllGate(t *testing.T) {
	plan := makeSimplePlan("node1", "read_file", nil)
	vCtx := &dag.DAGValidationContext{
		Plan:       plan,
		PolicyGate: &denyAllGate{},
		AgentID:    "test-agent",
	}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("denyAllGate should block execution")
	}
	var valErr *dag.DAGValidationError
	if errors.As(err, &valErr) && valErr.Layer != "L1_policy" {
		t.Errorf("expected L1_policy error, got %s", valErr.Layer)
	}
}

func TestValidateDAG_L1Policy_ErrorGate_FailClosed(t *testing.T) {
	// PolicyGate 出错 → fail-closed 拒绝
	plan := makeSimplePlan("node1", "read_file", nil)
	vCtx := &dag.DAGValidationContext{
		Plan:       plan,
		PolicyGate: &errorGate{},
		AgentID:    "test-agent",
	}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("PolicyGate error should fail-closed")
	}
	var valErr *dag.DAGValidationError
	if errors.As(err, &valErr) && valErr.Layer != "L1_policy" {
		t.Errorf("expected L1_policy error, got %s", valErr.Layer)
	}
}

func TestValidateDAG_L1Policy_NilGate_FailClosed(t *testing.T) {
	// PolicyGate 为 nil → fail-closed 拒绝（保护线）
	plan := makeSimplePlan("node1", "read_file", nil)
	vCtx := &dag.DAGValidationContext{
		Plan:       plan,
		PolicyGate: nil,
		AgentID:    "test-agent",
	}
	err := dag.ValidateDAG(context.Background(), vCtx)
	if err == nil {
		t.Error("nil PolicyGate should fail-closed")
	}
}

// ─── 集成测试：Agent 层调用 runValidateDAG ────────────────────────────────────

func TestAgent_RunValidateDAG_WithPolicyGate(t *testing.T) {
	agent := NewAgentWithDefaults("agent-validate-test")
	agent.InjectPolicyGate(&allowAllGate{})
	// fsm.DAGModel 为 nil 时，dag.ValidateDAG 应接受（nil plan → L0 拦截，但我们测试 nil fsm.DAGModel 的处理）
	// 注意：nil plan 会被 L0 拦截（dag.ValidateDAG 返回 L0 错误）。
	// runValidateDAG 会发出 TriggerValidateFail。
	// 这里直接调用 runValidateDAG 而非 Run 循环，验证它不 panic。
	err := agent.runValidateDAG(context.Background())
	// nil fsm.DAGModel → nil plan → L0 拦截 → 返回非 nil error（但 trigger 已发出）
	if err == nil {
		t.Error("nil fsm.DAGModel should return a validation error")
	}
}

func TestAgent_RunValidateDAG_ValidPlan_AllowAll(t *testing.T) {
	agent := NewAgentWithDefaults("agent-validate-pass")
	agent.InjectPolicyGate(&allowAllGate{})
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
	err := agent.runValidateDAG(context.Background())
	if err != nil {
		t.Errorf("valid plan with allowAllGate should pass: %v", err)
	}
}

func TestAgent_RunValidateDAG_ValidPlan_DenyAll(t *testing.T) {
	agent := NewAgentWithDefaults("agent-validate-deny")
	agent.InjectPolicyGate(&denyAllGate{})
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{{ID: "n1", ToolName: "read_file"}},
	}
	err := agent.runValidateDAG(context.Background())
	if err == nil {
		t.Error("denyAllGate should cause validate to fail")
	}
}
