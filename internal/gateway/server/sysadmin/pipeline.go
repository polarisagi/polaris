package sysadmin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandlePipelineRun 触发一条 Agent 流水线执行。
func (h *SysAdminHandler) HandlePipelineRun(w http.ResponseWriter, r *http.Request) {
	if h.PipelineOrch == nil {
		http.Error(w, "pipeline orchestrator not enabled", http.StatusServiceUnavailable)
		return
	}

	var req types.PipelineDescriptor
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid pipeline descriptor format", http.StatusBadRequest)
		return
	}

	res, err := h.PipelineOrch.Run(context.Background(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, res)
}
