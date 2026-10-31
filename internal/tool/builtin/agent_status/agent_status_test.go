package agent_status

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockStatusMgr struct {
	queriedIDs []string
}

func (m *mockStatusMgr) SpawnAgents(_ context.Context, _ types.SpawnAgentsArgs) ([]string, error) {
	return nil, nil
}
func (m *mockStatusMgr) AgentStatus(_ context.Context, ids []string) ([]types.SubagentStatusInfo, error) {
	m.queriedIDs = ids
	return []types.SubagentStatusInfo{
		{ID: "sub-1", Role: "researcher", Status: "running"},
		{ID: "sub-2", Role: "coder", Status: "done"},
	}, nil
}
func (m *mockStatusMgr) AgentResult(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockStatusMgr) AgentResultWithTaint(_ context.Context, _ string) (string, types.TaintLevel, error) {
	return "", types.TaintHigh, nil
}
func (m *mockStatusMgr) AgentMessage(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *mockStatusMgr) AgentStop(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestAgentStatus_Fn(t *testing.T) {
	fn := MakeAgentStatusFn()
	mgr := &mockStatusMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	input := []byte(`{"ids":["sub-1","sub-2"]}`)
	outBytes, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if int(res["count"].(float64)) != 2 {
		t.Errorf("expected count=2, got %v", res["count"])
	}
	if len(mgr.queriedIDs) != 2 {
		t.Errorf("expected 2 queried IDs, got %v", mgr.queriedIDs)
	}
}

func TestAgentStatus_MissingManager(t *testing.T) {
	fn := MakeAgentStatusFn()
	_, err := fn(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when SubagentManager is missing")
	}
}
