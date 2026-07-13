package sysadmin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
)

// HandlePatternDAGRun 触发一条 DAG 编排任务。
func (h *SysAdminHandler) HandlePatternDAGRun(w http.ResponseWriter, r *http.Request) {
	if h.PatternDAGExec == nil {
		http.Error(w, "pattern DAG executor not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string                     `json:"parent_task_id"`
		Spec         protocol.WorkflowGraphSpec `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid DAG descriptor format", http.StatusBadRequest)
		return
	}

	err := h.PatternDAGExec.Execute(context.Background(), req.ParentTaskID, req.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}
