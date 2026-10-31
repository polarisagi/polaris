package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
)

// HandlePatternDAGRun 触发一条 DAG 编排任务。
func (h *SysAdminHandler) HandlePatternDAGRun(w http.ResponseWriter, r *http.Request) {
	if h.PatternDAGExec == nil {
		httputil.RespondError(w, "pattern DAG executor not enabled", nil, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string                     `json:"parent_task_id"`
		Spec         protocol.WorkflowGraphSpec `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid DAG descriptor format", nil, http.StatusBadRequest)
		return
	}

	err := h.PatternDAGExec.Execute(r.Context(), req.ParentTaskID, req.Spec)
	if err != nil {
		httputil.RespondError(w, "DAG task failed", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, map[string]string{"status": "started"})
}
