package agent_message

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockMessageMgr struct {
	sentID   string
	sentText string
}

func (m *mockMessageMgr) SpawnAgents(_ context.Context, _ types.SpawnAgentsArgs) ([]string, error) {
	return nil, nil
}
func (m *mockMessageMgr) AgentStatus(_ context.Context, _ []string) ([]types.SubagentStatusInfo, error) {
	return nil, nil
}
func (m *mockMessageMgr) AgentResult(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (m *mockMessageMgr) AgentResultWithTaint(_ context.Context, _ string) (string, types.TaintLevel, error) {
	return "", types.TaintHigh, nil
}
func (m *mockMessageMgr) AgentMessage(_ context.Context, id, text string) (string, error) {
	m.sentID = id
	m.sentText = text
	return "ok", nil
}
func (m *mockMessageMgr) AgentStop(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestAgentMessage_Fn(t *testing.T) {
	fn := MakeAgentMessageFn()
	mgr := &mockMessageMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	input := []byte(`{"id":"sub-99","text":"focus on testing"}`)
	outBytes, err := fn(ctx, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "sent" || res["agent_id"] != "sub-99" {
		t.Errorf("unexpected output: %v", res)
	}
	if mgr.sentID != "sub-99" || mgr.sentText != "focus on testing" {
		t.Errorf("mgr not called correctly: %+v", mgr)
	}
}

func TestAgentMessage_MissingFields(t *testing.T) {
	fn := MakeAgentMessageFn()
	mgr := &mockMessageMgr{}
	ctx := context.WithValue(context.Background(), protocol.CtxSubagentManagerKey{}, mgr)

	_, err := fn(ctx, []byte(`{"id":"sub-99"}`))
	if err == nil {
		t.Fatal("expected error when text is empty")
	}
}
