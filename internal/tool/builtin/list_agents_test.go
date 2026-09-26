package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeA2AAgentLister struct {
	descriptors []A2AAgentDescriptor
	err         error
}

func (f *fakeA2AAgentLister) ListA2AAgents(context.Context) ([]A2AAgentDescriptor, error) {
	return f.descriptors, f.err
}

type fakeLocalAgentLister []LocalAgentDescriptor

func (f fakeLocalAgentLister) ListLocalAgents(context.Context) ([]LocalAgentDescriptor, error) {
	return f, nil
}

func runListAgents(t *testing.T, local LocalAgentLister, a2a A2AAgentLister) []listAgentsResult {
	t.Helper()
	out, err := MakeListAgentsFn(local, a2a)(context.Background(), nil)
	if err != nil {
		t.Fatalf("list_agents: %v", err)
	}
	var results []listAgentsResult
	if uerr := json.Unmarshal(out, &results); uerr != nil {
		t.Fatalf("invalid JSON: %v (%s)", uerr, out)
	}
	return results
}

func TestListAgents_NilListersListGeneralPurposeOnly(t *testing.T) {
	got := runListAgents(t, nil, nil)
	if len(got) != 1 || got[0].Target != GeneralPurposeAgent {
		t.Fatalf("got %+v", got)
	}
}

func TestListAgents_LocalAndMCPTargets(t *testing.T) {
	got := runListAgents(t,
		fakeLocalAgentLister{{Name: "review:security", Description: "security reviewer"}},
		&fakeA2AAgentLister{descriptors: []A2AAgentDescriptor{{Server: "linear", Agent: "researcher", Description: "research agent"}}})
	if len(got) != 3 || got[1].Target != "review:security" || got[1].Kind != "local" ||
		got[2].Target != "mcp:linear/researcher" || got[2].Kind != "mcp" {
		t.Fatalf("got %+v", got)
	}
}

func TestListAgents_ListerErrorPropagates(t *testing.T) {
	if _, err := MakeListAgentsFn(nil, &fakeA2AAgentLister{err: errors.New("boom")})(context.Background(), nil); err == nil {
		t.Fatal("expected error to propagate when ListA2AAgents fails")
	}
}

func TestTransferToAgentTool_DirectCallFails(t *testing.T) {
	if transferToAgentTool().Name != "transfer_to_agent" || listAgentsTool().Name != "list_agents" {
		t.Fatal("tool names are part of the kernel contract")
	}
	if _, err := transferToAgentFn(context.Background(), nil); err == nil {
		t.Fatal("direct dispatch of transfer_to_agent must fail")
	}
}
