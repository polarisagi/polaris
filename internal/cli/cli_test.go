package cli

import (
	"context"
	"testing"
	"time"
)

func TestAgentREPL_AddHistory(t *testing.T) {
	repl := &AgentREPL{}
	repl.AddHistory(REPLEntry{Input: "test", Output: "out"})
	if len(repl.history) != 1 {
		t.Errorf("Expected 1 history entry")
	}
	if repl.history[0].Input != "test" {
		t.Errorf("Expected test")
	}
}

func TestAgentREPL_SetSession(t *testing.T) {
	repl := &AgentREPL{}
	s := &Session{ID: "s1"}
	repl.SetSession(s)
	if repl.session.ID != "s1" {
		t.Errorf("Expected s1")
	}
}

func TestAgentREPL_HandleCommand(t *testing.T) {
	repl := &AgentREPL{session: &Session{ID: "s1"}}

	if !repl.handleCommand(context.Background(), "/quit") {
		t.Errorf("Expected /quit to return true")
	}
	if !repl.handleCommand(context.Background(), "/exit") {
		t.Errorf("Expected /exit to return true")
	}
	if repl.handleCommand(context.Background(), "/sessions") {
		t.Errorf("Expected /sessions to return false")
	}
	if repl.handleCommand(context.Background(), "/status") {
		t.Errorf("Expected /status to return false")
	}
	if repl.handleCommand(context.Background(), "/unknown") {
		t.Errorf("Expected /unknown to return false")
	}
}

func TestRateLimiterMiddleware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rlm := NewRateLimiterMiddleware(ctx)

	// Without limit, admit should be true
	if !rlm.Admit("fp1", "cli") {
		t.Errorf("Expected admit true for unconfigured key")
	}

	rlm.mu.Lock()
	rlm.limits["fp2:cli"] = &RateLimit{QuotaPerSec: 10, BurstAllow: 2}
	rlm.mu.Unlock()

	// First admit should pass
	if !rlm.Admit("fp2", "cli") {
		t.Errorf("Expected admit true")
	}
}

func TestWebSocketHub(t *testing.T) {
	hub := NewWebSocketHub()
	ctx, cancel := context.WithCancel(context.Background())

	hub.Run(ctx)

	client := &WSClient{ID: "c1", Send: make(chan WSEvent, 10)}
	hub.register <- client

	// wait for register
	time.Sleep(50 * time.Millisecond)

	hub.broadcast <- WSEvent{Type: "test"}

	select {
	case ev := <-client.Send:
		if ev.Type != "test" {
			t.Errorf("Expected test event")
		}
	case <-time.After(100 * time.Millisecond):
		t.Errorf("Expected broadcast event")
	}

	hub.unregister <- client
	time.Sleep(50 * time.Millisecond)

	cancel()
}

func TestCoalesceEvents(t *testing.T) {
	hub := NewWebSocketHub()
	events := []WSEvent{
		{Type: "token"},
		{Type: "token"},
		{Type: "error"},
	}

	res := hub.CoalesceEvents(events)
	if len(res) != 2 {
		t.Errorf("Expected 2 events after coalesce, got %d", len(res))
	}
}
