package orchestrator

import (
	"testing"
)

func TestAgentRegistry_RegisterDeregister(t *testing.T) {
	r := NewAgentRegistry()

	card := AgentCard{
		Name: "test-agent",
	}

	err := r.Register("agent-1", card, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handle, ok := r.Get("agent-1")
	if !ok || handle.Status != "active" {
		t.Errorf("expected agent to be active")
	}

	r.MarkUnreachable("agent-1")
	handle, _ = r.Get("agent-1")
	if handle.Status != "unreachable" {
		t.Errorf("expected agent to be unreachable, got %s", handle.Status)
	}

	r.Deregister("agent-1")
	_, ok = r.Get("agent-1")
	if ok {
		t.Errorf("expected agent to be deregistered")
	}
}
