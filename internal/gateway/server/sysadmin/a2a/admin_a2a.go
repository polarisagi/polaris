package a2a

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// AgentCardHandler GET /.well-known/agent-card.json
// 提供 A2A v0.3 Agent Card 静态信息
func AgentCardHandler(cfg config.A2AConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		card := orchestrator.AgentCard{
			Name:    cfg.Name,
			Version: "v0.3.0",
			Skills:  cfg.Skills,
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}
}

// A2ATaskRequest 定义外部 A2A 任务请求体 (JSON-RPC 2.0 风格)
type A2ATaskRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// TaskSubmitHandler POST /v1/a2a/tasks
// 接收外部 Agent 的任务委派，投递至 Blackboard
func TaskSubmitHandler(bb protocol.Blackboard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req A2ATaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON-RPC body", http.StatusBadRequest)
			return
		}

		if req.JSONRPC != "2.0" || req.Method != "delegate_task" {
			http.Error(w, "invalid method or jsonrpc version", http.StatusBadRequest)
			return
		}

		taskID := req.ID
		if taskID == "" {
			taskID = uuid.NewString()
		}

		// GD-14-003: 强制 TaintHigh；外部 Agent 参数序列化为 Intent 字节流
		entry := &types.TaskEntry{
			ID:          taskID,
			Type:        "a2a_delegation",
			Status:      types.TaskPending,
			Priority:    1,
			IntentTaint: types.TaintHigh,
			Intent:      req.Params,
		}

		if err := bb.PostTask(context.Background(), entry); err != nil {
			http.Error(w, "failed to post task", http.StatusInternalServerError)
			return
		}

		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      taskID,
			"result": map[string]string{
				"status": "accepted",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
