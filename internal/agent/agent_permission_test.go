package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// 未注入权限来源（单元测试/旧装配）时 authorizePlan 不介入，保留原有污点复核路径。
func TestAuthorizePlan_NoopWithoutPermissionSource(t *testing.T) {
	a := NewAgent("a1", nil, nil)
	vCtx := &protocol.DAGValidationContext{Plan: &protocol.DAGPlan{Nodes: []protocol.ExecNode{
		{ID: "n1", ToolName: "bash", Args: []byte(`{"command":"rm -rf /"}`)},
	}}}
	if err := a.authorizePlan(context.Background(), vCtx); err != nil {
		t.Fatalf("authorizePlan must be a no-op without a permission source, got %v", err)
	}
}

// 没有审批通道时，需要询问的调用必须拒绝（fail-closed），且原因回灌给模型。
func TestAuthorizePlan_NoApprovalChannelDenies(t *testing.T) {
	a := NewAgentWithDefaults("a1")
	a.InjectPermission(newPermMode(types.ModeAutoReview), nil)
	a.workspaceRoot = t.TempDir()
	vCtx := &protocol.DAGValidationContext{Plan: &protocol.DAGPlan{Nodes: []protocol.ExecNode{
		{ID: "n1", ToolName: "code_act:python", Args: []byte(`{"code":"print(1)"}`)},
	}}}
	err := a.authorizePlan(context.Background(), vCtx)
	ve, ok := err.(*protocol.DAGValidationError)
	if !ok || ve.Layer != "L1_policy" || ve.Reason == "" {
		t.Fatalf("expected L1_policy denial with reason, got %v", err)
	}
}

// 同一回合内被拒绝的调用不再重复询问。
func TestAuthorizePlan_DeniedCallIsNotAskedTwice(t *testing.T) {
	f := newPermFixture(t, types.ModeAutoReview, "p1", &memRules{}, nil, func(types.HITLPrompt) types.HITLResponse { return types.HITLResponse{} })
	vCtx := &protocol.DAGValidationContext{AgentID: "x", Plan: &protocol.DAGPlan{Nodes: []protocol.ExecNode{
		{ID: "n1", ToolName: "bash", Args: []byte(`{"command":"frobnicate"}`)},
	}}}
	for range 2 {
		if err := f.ag.authorizePlan(context.Background(), vCtx); err == nil {
			t.Fatal("denied call must stay denied")
		}
	}
	if f.hitl.count() != 1 {
		t.Fatalf("a declined call must be asked once per turn, got %d", f.hitl.count())
	}
}
