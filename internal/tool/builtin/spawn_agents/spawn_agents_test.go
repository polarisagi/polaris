package spawn_agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockSubagentMgr struct {
	spawned []types.SpawnTaskSpec
	wait    string
}

func (m *mockSubagentMgr) SpawnAgents(_ context.Context, args types.SpawnAgentsArgs) ([]string, error) {
	m.spawned = args.Tasks
	m.wait = args.Wait
	ids := make([]string, len(args.Tasks))
	for i := range args.Tasks {
		ids[i] = "agent-" + string(rune('1'+i))
	}
	return ids, nil
}

func (m *mockSubagentMgr) AgentStatus(_ context.Context, _ []string) ([]types.SubagentStatusInfo, error) {
	return nil, nil
}
func (m *mockSubagentMgr) AgentResult(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockSubagentMgr) AgentResultWithTaint(_ context.Context, _ string) (string, types.TaintLevel, error) {
	return "", types.TaintHigh, nil
}
func (m *mockSubagentMgr) AgentMessage(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *mockSubagentMgr) AgentStop(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestSpawnAgents_Fn(t *testing.T) {
	fn := MakeSpawnAgentsFn()
	mgr := &mockSubagentMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	input := []byte(`{"tasks":[{"prompt":"task A","role":"worker"},{"prompt":"task B"}],"wait":"all"}`)
	outBytes, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "dispatched" {
		t.Errorf("expected status=dispatched, got %v", res["status"])
	}
	if int(res["count"].(float64)) != 2 {
		t.Errorf("expected count=2, got %v", res["count"])
	}
	if mgr.wait != "all" || len(mgr.spawned) != 2 {
		t.Errorf("mgr not called correctly: %+v", mgr)
	}
}

func TestSpawnAgents_MissingManager(t *testing.T) {
	fn := MakeSpawnAgentsFn()
	_, err := fn(context.Background(), []byte(`{"tasks":[{"prompt":"task A"}]}`))
	if err == nil {
		t.Fatal("expected error when SubagentManager is missing")
	}
}
