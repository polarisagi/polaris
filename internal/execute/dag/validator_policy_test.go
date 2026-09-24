package dag

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

type trustLookupExecutor struct{ trust types.TrustTier }

func (e trustLookupExecutor) ExecuteWithTaint(context.Context, string, []byte, types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{}, nil
}

func (e trustLookupExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, TrustTier: e.trust}, nil
}

// TestValidatePolicyGate_AsksSameQuestionAsExecEnvelope 复现 2026-09-25 实测：L1 以
// "工具名"作 action、会话 ID 作 principal 发问，策略模型里没有任何规则认识这种请求，
// 真实 Gate 对所有工具一律 "denied by default"——而执行闸门（sandbox.ExecEnvelope）
// 问的是 (agent, tool_execute, <tool>, trust_tier...)。预检与执行必须问同一个问题。
func TestValidatePolicyGate_AsksSameQuestionAsExecEnvelope(t *testing.T) {
	gate := policy.NewGate(func() {})
	plan := &protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "sys_probe"}}}

	official := &DAGValidationContext{Plan: plan, PolicyGate: gate, AgentID: "sess_x",
		ToolExecutor: trustLookupExecutor{trust: types.TrustOfficial}}
	if err := validatePolicyGate(context.Background(), official); err != nil {
		t.Fatalf("内置/官方工具（trust>=3）应通过 L1，与执行闸门 tool_execute_permit 一致: %v", err)
	}

	community := &DAGValidationContext{Plan: plan, PolicyGate: gate, AgentID: "sess_x",
		ToolExecutor: trustLookupExecutor{trust: types.TrustCommunity}}
	if err := validatePolicyGate(context.Background(), community); err == nil {
		t.Fatal("trust<3 且无能力令牌的工具必须被拒（与执行闸门一致，deny-by-default）")
	}

	unknown := &DAGValidationContext{Plan: plan, PolicyGate: gate, AgentID: "sess_x"}
	if err := validatePolicyGate(context.Background(), unknown); err == nil {
		t.Fatal("拿不到工具元数据时必须 fail-closed")
	}
}
