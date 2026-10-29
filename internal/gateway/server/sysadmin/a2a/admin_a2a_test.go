package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	// 任务 ID 由本节点生成，不再采用调用方 JSON-RPC id（ADR-0111：撞键会被 INSERT OR IGNORE 静默丢弃）。
	task := bb.tasks[0]
	if task.ID == "123" || !strings.HasPrefix(task.ID, "a2a-legacy-") {
		t.Errorf("task ID must be node-generated, got %s", task.ID)
	}
	if task.SpawnDepth != 1 {
		t.Errorf("legacy inbound must count as first hop (SpawnDepth=1), got %d", task.SpawnDepth)
	}
	var resp struct {
		ID     string            `json:"id"`
		Result map[string]string `json:"result"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "123" || resp.Result["task_id"] != task.ID {
		t.Errorf("response must echo the JSON-RPC id and return the node task id, got %+v", resp)
	}
	if task.IntentTaint != types.TaintHigh {
		t.Errorf("expected taint high, got %d", task.IntentTaint)
	}
}
