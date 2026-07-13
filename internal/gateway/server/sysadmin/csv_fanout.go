package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
)

// HandleCSVFanout 触发 CSV Fan-out 任务
func (h *SysAdminHandler) HandleCSVFanout(w http.ResponseWriter, r *http.Request) {
	if h.Blackboard == nil {
		http.Error(w, "Blackboard is not available", http.StatusServiceUnavailable)
		return
	}

	var job orchestrator.CSVFanoutJob
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// TODO: support event logger
	res, err := orchestrator.RunCSVFanout(r.Context(), h.Blackboard, job)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
