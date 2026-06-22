package topology

import (
	"context"
	"testing"
)

// ── CapabilityRegistry ────────────────────────────────────────────────────────

func TestRegistry_Register_And_Find(t *testing.T) {
	r := NewCapabilityRegistry()
	err := r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"search", "write"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ids := r.FindAgents([]Capability{"search"})
	if len(ids) != 1 || ids[0] != "a1" {
		t.Errorf("expected [a1], got %v", ids)
	}
}

func TestRegistry_Find_MultiCapIntersection(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"search", "write"}})
	r.Register(AgentCapabilities{AgentID: "a2", Capabilities: []Capability{"search"}})

	ids := r.FindAgents([]Capability{"search", "write"})
	if len(ids) != 1 || ids[0] != "a1" {
		t.Errorf("expected only a1 for search+write, got %v", ids)
	}
}

func TestRegistry_Find_NoMatch(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"search"}})

	ids := r.FindAgents([]Capability{"fly"})
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

func TestRegistry_Find_Empty_Caps_Returns_All(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"x"}})
	r.Register(AgentCapabilities{AgentID: "a2", Capabilities: []Capability{"y"}})

	ids := r.FindAgents(nil)
	if len(ids) != 2 {
		t.Errorf("empty caps should return all agents, got %v", ids)
	}
}

func TestRegistry_Register_Renew(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"old"}})
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"new"}}) // renew

	if ids := r.FindAgents([]Capability{"old"}); len(ids) != 0 {
		t.Errorf("old cap should be replaced, got %v", ids)
	}
	if ids := r.FindAgents([]Capability{"new"}); len(ids) != 1 {
		t.Errorf("expected a1 under new cap, got %v", ids)
	}
}

func TestRegistry_ErrRegistryFull(t *testing.T) {
	r := NewCapabilityRegistry()
	r.SetMaxCapacity(2)
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"x"}})
	r.Register(AgentCapabilities{AgentID: "a2", Capabilities: []Capability{"y"}})

	err := r.Register(AgentCapabilities{AgentID: "a3", Capabilities: []Capability{"z"}})
	if err != ErrRegistryFull {
		t.Errorf("expected ErrRegistryFull, got %v", err)
	}
}

func TestRegistry_ErrDuplicateCapabilities(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"x", "y"}})

	err := r.Register(AgentCapabilities{AgentID: "a2", Capabilities: []Capability{"x", "y"}})
	if err != ErrDuplicateCapabilities {
		t.Errorf("expected ErrDuplicateCapabilities, got %v", err)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"search"}})
	r.Unregister("a1")

	if r.AgentCount() != 0 {
		t.Errorf("expected 0 agents after unregister, got %d", r.AgentCount())
	}
	if ids := r.FindAgents([]Capability{"search"}); len(ids) != 0 {
		t.Errorf("unregistered agent should not appear in FindAgents, got %v", ids)
	}
}

func TestRegistry_LoadSorting(t *testing.T) {
	r := NewCapabilityRegistry()
	// 两 Agent 各持独特第二能力，使能力集合不同，通过重复能力检测
	r.Register(AgentCapabilities{AgentID: "heavy", Capabilities: []Capability{"cap", "heavy-only"}})
	r.Register(AgentCapabilities{AgentID: "light", Capabilities: []Capability{"cap", "light-only"}})
	r.UpdateLoad("heavy", 5)
	r.UpdateLoad("light", 1)

	ids := r.FindAgents([]Capability{"cap"})
	if len(ids) < 2 || ids[0] != "light" {
		t.Errorf("expected light (load=1) first, got %v", ids)
	}
}

func TestRegistry_AcquireReleaseLease(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"cap"}})

	r.AcquireLease("a1")
	r.AcquireLease("a1")
	ids := r.FindAgents([]Capability{"cap"})
	if len(ids) == 0 {
		t.Fatal("agent should still be found")
	}

	r.ReleaseLease("a1")
	r.ReleaseLease("a1")
	r.ReleaseLease("a1") // extra release should not go below 0
}

// ── SwarmRouter ───────────────────────────────────────────────────────────────

func TestSwarmRouter_Disabled_ReturnsHierarchy(t *testing.T) {
	r := NewCapabilityRegistry()
	router := NewSwarmRouter(false, r, nil)

	result, err := router.RouteTask(context.Background(), "intent", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != TopologyHierarchy {
		t.Errorf("disabled router should return Hierarchy, got %v", result.Mode)
	}
}

func TestSwarmRouter_HierarchyNilPublisher_Degrades(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"x"}})
	router := NewSwarmRouter(true, r, nil)

	result, err := router.RouteTask(context.Background(), "do something", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != TopologyHierarchy {
		t.Errorf("nil publisher should degrade to Hierarchy, got %v", result.Mode)
	}
}

func TestSwarmRouter_AutoUpgradeToMesh(t *testing.T) {
	r := NewCapabilityRegistry()
	router := NewSwarmRouter(true, r, nil)

	// 注册超过 meshThreshold 个 Agent（每个 cap 不同，避免重复错误）
	for i := 0; i < meshThreshold; i++ {
		cap := Capability(string(rune('a' + i)))
		r.Register(AgentCapabilities{AgentID: string(rune('a'+i)) + "-agent", Capabilities: []Capability{cap}})
	}

	router.RouteTask(context.Background(), "intent", nil)
	if router.CurrentMode != TopologyMesh {
		t.Errorf("expected Mesh after %d agents, got %v", meshThreshold, router.CurrentMode)
	}
}

func TestSwarmRouter_MeshRoute_ReturnsAgentIDs(t *testing.T) {
	r := NewCapabilityRegistry()
	router := NewSwarmRouter(true, r, nil)

	// 注册 meshThreshold 个 Agent 触发自动升级到 Mesh，每个 cap 独特
	for i := range meshThreshold {
		cap := Capability(string(rune('a' + i)))
		r.Register(AgentCapabilities{
			AgentID:      string(rune('a'+i)) + "-agent",
			Capabilities: []Capability{cap, "shared"},
		})
	}

	result, err := router.RouteTask(context.Background(), "", []Capability{"shared"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != TopologyMesh {
		t.Errorf("expected Mesh mode with %d agents, got %v", meshThreshold, result.Mode)
	}
	if len(result.AgentIDs) == 0 {
		t.Error("expected at least one agent ID")
	}
}

func TestSwarmRouter_MeshRoute_NoMatchFallsBack(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(AgentCapabilities{AgentID: "a1", Capabilities: []Capability{"search"}})
	router := NewSwarmRouter(true, r, nil)
	router.SetMode(TopologyMesh)

	result, err := router.RouteTask(context.Background(), "", []Capability{"fly"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != TopologyHierarchy {
		t.Errorf("no-match mesh should fall back to Hierarchy, got %v", result.Mode)
	}
}
