package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockBlackboard struct {
	protocol.Blackboard
	tasks []*types.TaskEntry
}

func (m *mockBlackboard) PostTask(ctx context.Context, entry *types.TaskEntry) error {
	m.tasks = append(m.tasks, entry)
	return nil
}

func TestAgentCardHandler(t *testing.T) {
	cfg := config.A2AConfig{
		Enabled: true,
		Name:    "test-agent",
		Skills:  []string{"test-skill"},
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()

	handler := AgentCardHandler(cfg)
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var card orchestrator.AgentCard
	if err := json.NewDecoder(w.Body).Decode(&card); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if card.Name != cfg.Name {
		t.Errorf("expected name %q, got %q", cfg.Name, card.Name)
	}
	if len(card.Skills) != 1 || card.Skills[0] != cfg.Skills[0] {
		t.Errorf("expected skills %v, got %v", cfg.Skills, card.Skills)
	}
}

func TestTaskSubmitHandler(t *testing.T) {
	bb := &mockBlackboard{}
	handler := TaskSubmitHandler(bb)

	reqBody := `{"jsonrpc":"2.0", "id":"123", "method":"delegate_task", "params": {"query": "hello"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/a2a/tasks", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d", http.StatusAccepted, w.Code)
	}

	if len(bb.tasks) != 1 {
		t.Fatalf("expected 1 task posted, got %d", len(bb.tasks))
	}

	task := bb.tasks[0]
	if task.ID != "123" {
		t.Errorf("expected task ID 123, got %s", task.ID)
	}
	if task.IntentTaint != types.TaintHigh {
		t.Errorf("expected taint high, got %d", task.IntentTaint)
	}
}
