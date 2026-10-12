package a2a

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// A2ATaskRequest 定义外部 A2A 任务请求体 (JSON-RPC 2.0 风格)
type A2ATaskRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// TaskSubmitHandler POST /v1/a2a/tasks —— 已弃用的私有协议（方法名 delegate_task），
// 由 A2A 1.0 门面 /a2a/v1 取代（ADR-0111 决策二）。只在 [a2a] legacy_endpoint=true 时注册，
// 保留一个版本供旧调用方迁移，每次调用打 deprecation 日志。
func TaskSubmitHandler(bb protocol.Blackboard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("a2a: deprecated endpoint POST /v1/a2a/tasks called; migrate to A2A 1.0 at POST /a2a/v1 (ADR-0111)",
			"remote", r.RemoteAddr)
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

		// 任务 ID 一律本节点生成（ADR-0111 决策二第 3 条）：此前直接用调用方的 JSON-RPC id，
		// 撞上已有任务时被 INSERT OR IGNORE 静默丢弃，却仍回 accepted。
		taskID := "a2a-legacy-" + uuid.NewString()

		// GD-14-003: 强制 TaintHigh；外部 Agent 参数序列化为 Intent 字节流
		entry := &types.TaskEntry{
			ID:          taskID,
			Type:        "a2a_delegation",
			Status:      types.TaskPending,
			Priority:    1,
			IntentTaint: types.TaintHigh,
			Intent:      req.Params,
			// 旧协议不携带委派深度：按外部调用方的第一跳处理（ADR-0111 决策四第 2 条），
			// 此前恒为 0，跨节点 A→B→A 委派链的深度上限对本入口失效。
			SpawnDepth: 1,
		}

		if err := bb.PostTask(context.Background(), entry); err != nil {
			http.Error(w, "failed to post task", http.StatusInternalServerError)
			return
		}

		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]string{
				"status":  "accepted",
				"task_id": taskID,
			},
		}
		httputil.WriteJSONStatus(w, http.StatusAccepted, resp)
	}
}
