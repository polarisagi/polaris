package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/security/taint"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/execute/dag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockCodeActForExecuteDAG struct {
	lastReq *CodeActRequest
}

func (m *mockCodeActForExecuteDAG) Execute(ctx context.Context, req CodeActRequest) (*CodeActResult, error) {
	m.lastReq = &req
	return &CodeActResult{ExitCode: 0, Output: taint.NewTaintedString("mock output", taint.TaintSource{}, "test")}, nil
}

func (m *mockCodeActForExecuteDAG) CheckSyntax(code, lang string) error {
	return nil
}

func (m *mockCodeActForExecuteDAG) IsAvailable() bool {
	return true
}

// TestAgent_ExecuteDAG_F02_TaintLevel 守护「调用方自报的 taint_level 不得影响 CodeAct
// 的污点判定」。
//
// 2026-08-12（C-8）改写：原断言检查 CodeActRequest.TaintLevel == TaintHigh。该字段已
// 整体删除——它全仓零读取点，沙箱执行侧硬编码 TaintHigh，留着只会诱导后来者拿它做安全
// 判定。删除后该性质由**类型结构**保证（根本没有传递污点等级的通道），比运行时断言更强。
// 本用例保留并改为守护结构性质：DAG 节点 Args 里塞进恶意 "taint_level":0 时，
// CodeAct 仍被正常调用且请求体不携带任何调用方可控的污点字段。
// 若将来有人重新给 CodeActRequest 加回污点字段，本用例连同 tools/taint_typed_fields_check.go
// 会一起把这次回退暴露出来。
func TestAgent_ExecuteDAG_F02_TaintLevel(t *testing.T) {
	agent := NewAgentWithDefaults("test-agent")
	agent.InjectToolExecutor(&mockToolExecutor{})
	agent.InjectDAGRunner(&dummyDAGRunner{})

	// Inject CodeAct provider
	mockCA := &mockCodeActForExecuteDAG{}
	agent.codeAct = mockCA

	// Set DAGModel
	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{
			{
				ID:       "n1",
				ToolName: "code_act:python",                             // triggers codeAct execution branch
				Args:     []byte(`{"code":"print(1)","taint_level":0}`), // malicious override attempt
			},
		},
	}
	// Simulate validated plan setting High taint level
	agent.sCtx.GlobalTaintLevel = types.TaintHigh

	// Call runExecuteDAG
	_ = agent.runExecuteDAG(context.Background())

	// 恶意 args 未阻断执行，且请求体不含任何调用方可控的污点字段。
	if mockCA.lastReq == nil {
		t.Fatalf("expected CodeAct to be called")
	}
	if reflect.TypeOf(CodeActRequest{}).NumField() != 6 {
		t.Fatalf("CodeActRequest 字段数变化，请确认未重新引入调用方可控的污点字段（C-8）")
	}
	if _, ok := reflect.TypeOf(CodeActRequest{}).FieldByName("TaintLevel"); ok {
		t.Error("CodeActRequest 不得含 TaintLevel 字段：调用方自报值不可作为安全判定输入（C-8）")
	}
}

type mockMemoryListErr struct {
	mockMemoryForIntegration
}

func (m *mockMemoryListErr) ListEpisodicEvents(ctx context.Context, query types.EpisodicQuery) ([]types.ScoredEvent, error) {
	return nil, apperr.New(apperr.CodeInternal, "mock list error")
}

func TestAgent_ExecuteDAG_F03_ListEventsError(t *testing.T) {
	// 新增单测模拟 ListEpisodicEvents 返回 error，断言 runExecuteDAG 短路返回错误而非继续下发新 Action。
	agent := NewAgentWithDefaults("test-agent")
	agent.InjectToolExecutor(&mockToolExecutor{})
	agent.InjectDAGRunner(&dummyDAGRunner{})
	agent.memory = &mockMemoryListErr{}

	agent.sCtx.DAGModel = &fsm.DAGModel{
		Nodes: []dag.ExecNode{
			{
				ID:       "n1",
				ToolName: "non_idempotent_tool",
			},
		},
	}

	err := agent.runExecuteDAG(context.Background())
	if err == nil {
		t.Fatalf("expected error from runExecuteDAG due to ListEpisodicEvents failure")
	}
	if !strings.Contains(err.Error(), "2PC phase1: list episodic events failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

type dummyDAGRunner struct{}

func (d *dummyDAGRunner) Run(
	ctx context.Context,
	plan *protocol.DAGPlan,
	toolExecutor func(context.Context, string, []byte, types.TaintLevel) (*types.ToolResult, error),
	delayCallback func(context.Context, string, string, time.Duration) error,
	sessionID string,
	agentID string,
) ([]protocol.NodeResult, bool, error) {
	for _, n := range plan.Nodes {
		_, err := toolExecutor(ctx, n.ToolName, n.Args, types.TaintHigh)
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}
