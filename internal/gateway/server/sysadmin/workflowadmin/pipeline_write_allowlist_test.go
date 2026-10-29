package workflowadmin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/pkg/types"
)

const kbSaveTool = "mcp__polaris-knowledge-base__kb_save"

type allowlistToolCaller struct{ calls int }

func (c *allowlistToolCaller) Call(_ context.Context, _ string, _ []byte, _ bool) ([]byte, error) {
	c.calls++
	return []byte(`{}`), nil
}

type countingApprover struct{ n int }

func (a *countingApprover) Approve(_ context.Context, _ pipeline.ApprovalRequest) error {
	a.n++
	return nil
}

func allowlistExec() *mockAdapterExecutor {
	return &mockAdapterExecutor{tools: map[string]types.Tool{
		"knowledge_ingest":  {Name: "knowledge_ingest", Source: types.ToolBuiltin, Capability: types.CapWriteLocal},
		kbSaveTool:          {Name: kbSaveTool, Source: types.ToolMCP},
		"mcp__other__write": {Name: "mcp__other__write", Source: types.ToolMCP},
	}}
}

func validateTool(a toolMetaAdapter, tool, approval string) []pipeline.FieldError {
	spec := pipeline.Spec{
		Kind:    "pipeline",
		Name:    "t",
		Trigger: pipeline.Trigger{Type: "manual"},
		Steps:   []pipeline.StepSpec{{Key: "w", Kind: "tool", Tool: tool, Approval: approval, Args: json.RawMessage(`{}`)}},
	}
	return pipeline.Validate(spec, a, pipeline.Limits{MaxSteps: 20}, nil)
}

func TestToolMetaAdapter_WriteAllowlist(t *testing.T) {
	exec := allowlistExec()
	cfgList := []string{"knowledge_ingest", kbSaveTool}

	a := toolMetaAdapter{exec: exec, writeAllowlist: cfgList}
	for _, tool := range cfgList {
		if errs := validateTool(a, tool, ""); len(errs) != 0 {
			t.Errorf("allowlisted %s should validate without approval, got %v", tool, errs)
		}
		ap := &countingApprover{}
		tc := &allowlistToolCaller{}
		r := &pipeline.Runner{Tools: tc, Deps: a, Approver: ap}
		res := r.RunStep(context.Background(), pipeline.RunContext{RunID: "r1"}, pipeline.StepSpec{Key: "w", Kind: "tool", Tool: tool, Args: json.RawMessage(`{}`)})
		if res.Status != "ok" || ap.n != 0 || tc.calls != 1 {
			t.Errorf("allowlisted %s: status=%s approvals=%d calls=%d err=%v", tool, res.Status, ap.n, tc.calls, res.Err)
		}
	}

	// 白名单外写工具：validate 要求 hitl，运行时缺声明拒绝，声明后走审批。
	if errs := validateTool(a, "mcp__other__write", ""); len(errs) == 0 {
		t.Errorf("non-allowlisted write tool must require approval=hitl")
	}
	if errs := validateTool(a, "mcp__other__write", "hitl"); len(errs) != 0 {
		t.Errorf("hitl-declared write tool should validate, got %v", errs)
	}
	ap := &countingApprover{}
	r := &pipeline.Runner{Tools: &allowlistToolCaller{}, Deps: a, Approver: ap}
	rc := pipeline.RunContext{RunID: "r2"}
	if res := r.RunStep(context.Background(), rc, pipeline.StepSpec{Key: "w", Kind: "tool", Tool: "mcp__other__write", Args: json.RawMessage(`{}`)}); res.Status != "error" {
		t.Errorf("non-allowlisted write without hitl must fail at runtime, got %s", res.Status)
	}
	if res := r.RunStep(context.Background(), rc, pipeline.StepSpec{Key: "w", Kind: "tool", Tool: "mcp__other__write", Approval: "hitl", Args: json.RawMessage(`{}`)}); res.Status != "ok" || ap.n != 1 {
		t.Errorf("hitl write should be approved once, status=%s approvals=%d", res.Status, ap.n)
	}
}

func TestToolMetaAdapter_EmptyAllowlistRequiresApproval(t *testing.T) {
	a := toolMetaAdapter{exec: allowlistExec(), writeAllowlist: []string{}}
	for _, tool := range []string{"knowledge_ingest", kbSaveTool} {
		if errs := validateTool(a, tool, ""); len(errs) == 0 {
			t.Errorf("%s must require approval=hitl when allowlist is empty (no hardcoded name)", tool)
		}
		r := &pipeline.Runner{Tools: &allowlistToolCaller{}, Deps: a, Approver: &countingApprover{}}
		res := r.RunStep(context.Background(), pipeline.RunContext{RunID: "r3"}, pipeline.StepSpec{Key: "w", Kind: "tool", Tool: tool, Args: json.RawMessage(`{}`)})
		if res.Status != "error" {
			t.Errorf("%s without hitl must fail at runtime with empty allowlist, got %s", tool, res.Status)
		}
	}
}
