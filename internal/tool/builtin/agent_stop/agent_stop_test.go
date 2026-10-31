package agent_stop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockStopMgr struct {
	stoppedID string
}

func (m *mockStopMgr) SpawnAgents(_ context.Context, _ types.SpawnAgentsArgs) ([]string, error) {
	return nil, nil
}
func (m *mockStopMgr) AgentStatus(_ context.Context, _ []string) ([]types.SubagentStatusInfo, error) {
	return nil, nil
}
func (m *mockStopMgr) AgentResult(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockStopMgr) AgentResultWithTaint(_ context.Context, _ string) (string, types.TaintLevel, error) {
	return "", types.TaintHigh, nil
}
func (m *mockStopMgr) AgentMessage(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *mockStopMgr) AgentStop(_ context.Context, id string) (string, error) {
	m.stoppedID = id
	return "stopped", nil
}

func TestAgentStop_Fn(t *testing.T) {
	fn := MakeAgentStopFn()
	mgr := &mockStopMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	input := []byte(`{"id":"sub-terminate"}`)
	outBytes, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "stopped" || res["agent_id"] != "sub-terminate" {
		t.Errorf("unexpected output: %v", res)
	}
	if mgr.stoppedID != "sub-terminate" {
		t.Errorf("mgr not called with correct id: %s", mgr.stoppedID)
	}
}

func TestAgentStop_MissingID(t *testing.T) {
	fn := MakeAgentStopFn()
	mgr := &mockStopMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	_, err := fn(ctx, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error when id is empty")
	}
}
