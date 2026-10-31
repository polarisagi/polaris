package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
)

type captureExecutionIdentityAudit struct {
	meta map[string]any
}

type testExecutionAuditor struct{}

func (testExecutionAuditor) BeginExecution(context.Context, protocol.ExecutionAuditIntent) (string, error) {
	return "test-execution", nil
}
func (testExecutionAuditor) CompleteExecution(context.Context, string, bool, string) error {
	return nil
}

func (c *captureExecutionIdentityAudit) Log(_ context.Context, _ string, meta map[string]any) error {
	c.meta = meta
	return nil
}

func TestAgenticJITContextCarriesTrustedExecutionAuditIdentity(t *testing.T) {
	a := NewAgent("agent-fallback", nil, nil)
	a.sCtx.AgentID = "agent-from-state"
	a.sCtx.SessionID = "session-from-state"
	args := []byte(`{"path":"file.txt"}`)
	a.recordValidatedPlan(&protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "write_file", Args: args}}})
	ctx, revoke, err := a.withJITCapability(context.Background(), "write_file", args)
	if err != nil {
		t.Fatal(err)
	}
	defer revoke()

	logger := &captureExecutionIdentityAudit{}
	gate := security.NewExecutionAuditGate(logger)
	id, err := gate.BeginExecution(ctx, protocol.ExecutionAuditIntent{Tool: "write_file", Source: "agent", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.CompleteExecution(ctx, id, true, ""); err != nil {
		t.Fatal(err)
	}
	if logger.meta["agent_id"] != "agent-from-state" || logger.meta["session_id"] != "session-from-state" || logger.meta["task_id"] != "session-from-state" {
		t.Fatalf("audit identity must come from trusted Agent state context: %#v", logger.meta)
	}
}
