package agent_result

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockResultMgr struct {
	requestedID string
}

func (m *mockResultMgr) SpawnAgents(_ context.Context, _ types.SpawnAgentsArgs) ([]string, error) {
	return nil, nil
}
func (m *mockResultMgr) AgentStatus(_ context.Context, _ []string) ([]types.SubagentStatusInfo, error) {
	return nil, nil
}
func (m *mockResultMgr) AgentResult(_ context.Context, id string) (string, error) {
	m.requestedID = id
	return "result for " + id, nil
}
func (m *mockResultMgr) AgentResultWithTaint(_ context.Context, id string) (string, types.TaintLevel, error) {
	m.requestedID = id
	return "result for " + id, types.TaintUserReviewed, nil
}
func (m *mockResultMgr) AgentMessage(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *mockResultMgr) AgentStop(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestAgentResult_Fn(t *testing.T) {
	fn := MakeAgentResultFn()
	mgr := &mockResultMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	input := []byte(`{"id":"sub-42"}`)
	outBytes, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["agent_id"] != "sub-42" || res["result"] != "result for sub-42" {
		t.Errorf("unexpected output: %v", res)
	}
}

func TestAgentResult_RichPreservesReviewedChildTaint(t *testing.T) {
	fn := MakeAgentResultRichFn()
	mgr := &mockResultMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)
	result, err := fn(ctx, sandbox.SandboxSpec{Input: []byte(`{"id":"sub-42"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.TaintLevel != types.TaintUserReviewed {
		t.Fatalf("agent_result rich output must preserve child review taint, got %v", result.TaintLevel)
	}
}

func TestAgentResult_MissingID(t *testing.T) {
	fn := MakeAgentResultFn()
	mgr := &mockResultMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	_, err := fn(ctx, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when id is empty")
	}
}
