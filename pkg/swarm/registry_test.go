package swarm

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

func TestAgentRegistry_FindBestAgent(t *testing.T) {
	r := NewAgentRegistry()

	r.Register("agent-1", AgentCard{Skills: []string{"read", "write"}}, nil)
	r.Register("agent-2", AgentCard{Skills: []string{"read", "write", "execute"}}, nil)
	r.Register("agent-3", AgentCard{Skills: []string{"read"}}, nil) // Will fail phase 1

	reqCaps := []string{"read", "write"}

	// scenario 1: agent-1 has better success rate
	stats := map[string]AgentStats{
		"agent-1": {SuccessCount: 10, AttemptCount: 10},
		"agent-2": {SuccessCount: 1, AttemptCount: 10},
	}
	loads := map[string]int{
		"agent-1": 1,
		"agent-2": 1,
	}

	best, err := r.FindBestAgent(reqCaps, loads, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Although agent-2 has more skills, its success rate is worse, and load is same
	if len(best.Card.Skills) > 2 && best.Card.Skills[2] == "execute" { // which means it picked agent-2
		t.Errorf("expected agent-1, got agent-2")
	}

	// scenario 2: agent-1 is heavily loaded, agent-2 is free
	loads["agent-1"] = 100
	loads["agent-2"] = 0

	// recalculate because of caching map in memory
	_, err = r.FindBestAgent(reqCaps, loads, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// even with worse success rate, agent-2 is completely free while agent-1 is choked
	// Laplace for A1: 11 / 12 = 0.916. Score: 0.6*0.916 + 0.4*(1/100) = 0.549 + 0.004 = 0.553
	// Laplace for A2: 2 / 12 = 0.166. Score: 0.6*0.166 + 0.4*(1/1) = 0.099 + 0.4 = 0.499
	// Hmm, actually A1 still wins because the load factor weight (0.4) is not enough to overcome the massive success rate gap if A2 is very bad.
	// Let's make A2 slightly better.
	stats["agent-2"] = AgentStats{SuccessCount: 5, AttemptCount: 10}

	best3, err := r.FindBestAgent(reqCaps, loads, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Let's see if A2 wins now.
	// A1: 0.553
	// A2 Laplace: 6/12 = 0.5. Score: 0.6*0.5 + 0.4*1 = 0.3 + 0.4 = 0.7
	// A2 > A1!
	if len(best3.Card.Skills) < 3 || best3.Card.Skills[2] != "execute" { // it must pick agent-2
		t.Errorf("expected agent-2, got agent-1")
	}
}
