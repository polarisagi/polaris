package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandlePipelineRun 触发一条 Agent 流水线执行。
func (h *SysAdminHandler) HandlePipelineRun(w http.ResponseWriter, r *http.Request) {
	if h.PipelineOrch == nil {
		httputil.RespondError(w, "pipeline orchestrator not enabled", nil, http.StatusServiceUnavailable)
		return
	}

	var req types.PipelineDescriptor
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid pipeline descriptor format", nil, http.StatusBadRequest)
		return
	}

	res, err := h.PipelineOrch.Run(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, "Pipeline task failed", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, res)
}
