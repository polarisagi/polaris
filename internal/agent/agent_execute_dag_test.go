package agent

import (
	"context"
	"strings"
	"testing"
	"time"

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
	return &CodeActResult{ExitCode: 0, Output: []byte("mock output")}, nil
}

func (m *mockCodeActForExecuteDAG) CheckSyntax(code, lang string) error {
	return nil
}

func (m *mockCodeActForExecuteDAG) IsAvailable() bool {
	return true
}

func TestAgent_ExecuteDAG_F02_TaintLevel(t *testing.T) {
	// 新增单测：构造恶意 codeArgs.TaintLevel=0 但外部授信 taintLevel=High 的用例，断言实际执行使用的是 High
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

	// Verify CodeActRequest TaintLevel
	if mockCA.lastReq == nil {
		t.Fatalf("expected CodeAct to be called")
	}
	if mockCA.lastReq.TaintLevel != types.TaintHigh {
		t.Errorf("expected TaintLevel %v, got %v", types.TaintHigh, mockCA.lastReq.TaintLevel)
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
