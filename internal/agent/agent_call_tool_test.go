package agent

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func callToolAgent(t *testing.T) *Agent {
	t.Helper()
	reg := budgetRegistry(t)
	a := NewAgentWithDefaults("ct")
	a.InjectCatalog(budgetCatalog(t, reg))
	return a
}

// 解包保留 call ID，执行用调用的名称与参数换成内层工具（ADR-0120 决策三）。
func TestUnwrapCallTools_RewritesToInner(t *testing.T) {
	a := callToolAgent(t)
	calls := []types.InferToolCall{
		{ID: "c1", Name: "call_tool", Input: []byte(`{"name":"git_diff","arguments":{"staged":true}}`)},
		{ID: "c2", Name: "read_file", Input: []byte(`{"path":"/x"}`)},
	}
	out, bad := a.unwrapCallTools(calls)
	if len(bad) != 0 || len(out) != 2 {
		t.Fatalf("out=%v bad=%v", out, bad)
	}
	if out[0].ID != "c1" || out[0].Name != "git_diff" || string(out[0].Input) != `{"staged":true}` {
		t.Fatalf("解包错误: %+v", out[0])
	}
	if out[1].Name != "read_file" {
		t.Fatalf("非 call_tool 调用不应改动: %+v", out[1])
	}
}

func TestUnwrapCallTools_Rejects(t *testing.T) {
	a := callToolAgent(t)
	cases := map[string]string{
		`{"name":"call_tool","arguments":{}}`: "cannot call itself",
		`{"arguments":{}}`:                    "needs",
		`{"name":"git_dif","arguments":{}}`:   "closest: git_diff",
	}
	for in, want := range cases {
		_, bad := a.unwrapCallTools([]types.InferToolCall{{ID: "x", Name: "call_tool", Input: []byte(in)}})
		if len(bad) != 1 || !strings.Contains(bad[0].msg, want) {
			t.Errorf("%s: 期望错误含 %q，got %+v", in, want, bad)
		}
	}
}

// 解包后的闸门以内层工具名为主体：Profile 禁用的工具经 call_tool 也被拒绝，不存在绕过通道。
func TestUnwrapCallTools_ProfileGateSeesInnerName(t *testing.T) {
	a := callToolAgent(t)
	a.SetAgentProfile(&types.AgentProfileSpec{Name: "ro", DisallowedTools: []string{"git_commit"}})
	out, bad := a.unwrapCallTools([]types.InferToolCall{{ID: "c1", Name: "call_tool", Input: []byte(`{"name":"git_commit","arguments":{"message":"m"}}`)}})
	if len(bad) == 1 {
		return // 不可见的工具在解包阶段即被拒，同样满足"不可绕过"
	}
	plan := toolCallsToDAGPlan(out, types.TaintNone)
	if plan.Nodes[0].ToolName != "git_commit" {
		t.Fatalf("DAG 节点应为内层工具名，got %q", plan.Nodes[0].ToolName)
	}
	if err := a.checkPlanAgainstProfile(plan); err == nil {
		t.Fatal("Profile 禁用的内层工具经 call_tool 调用必须被拒")
	}
}
