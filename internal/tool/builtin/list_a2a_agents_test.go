package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeA2AAgentLister 是 A2AAgentLister 的测试替身。
type fakeA2AAgentLister struct {
	descriptors []A2AAgentDescriptor
	err         error
}

func (f *fakeA2AAgentLister) ListA2AAgents(ctx context.Context) ([]A2AAgentDescriptor, error) {
	return f.descriptors, f.err
}

func TestMakeListA2AAgentsFn_NilListerReturnsEmptyList(t *testing.T) {
	fn := MakeListA2AAgentsFn(nil)
	out, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	var results []listA2AAgentsResult
	if uerr := json.Unmarshal(out, &results); uerr != nil {
		t.Fatalf("expected valid JSON, got %v (raw: %s)", uerr, out)
	}
	if len(results) != 0 {
		t.Errorf("expected empty list when lister is nil, got %v", results)
	}
}

func TestMakeListA2AAgentsFn_FormatsTargetAsMCPPrefix(t *testing.T) {
	fn := MakeListA2AAgentsFn(&fakeA2AAgentLister{
		descriptors: []A2AAgentDescriptor{
			{Server: "linear", Agent: "researcher", Description: "research agent"},
		},
	})
	out, err := fn(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	var results []listA2AAgentsResult
	if uerr := json.Unmarshal(out, &results); uerr != nil {
		t.Fatalf("expected valid JSON, got %v", uerr)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Target != "mcp:linear/researcher" {
		t.Errorf("expected target 'mcp:linear/researcher' (directly usable as target_agent_role), got %q", results[0].Target)
	}
	if results[0].Description != "research agent" {
		t.Errorf("expected description passthrough, got %q", results[0].Description)
	}
}

func TestMakeListA2AAgentsFn_ListerErrorPropagates(t *testing.T) {
	fn := MakeListA2AAgentsFn(&fakeA2AAgentLister{err: errors.New("boom")})
	_, err := fn(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error to propagate when ListA2AAgents fails")
	}
}

func TestListA2AAgentsTool_Schema(t *testing.T) {
	tool := listA2AAgentsTool()
	if tool.Name != "list_a2a_agents" {
		t.Errorf("expected tool name 'list_a2a_agents', got %q", tool.Name)
	}
}
