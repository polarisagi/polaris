package sysadmin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type sysadminCSVEventLogger struct {
	agent protocol.AgentController
}

func (s sysadminCSVEventLogger) Append(ctx context.Context, ev types.Event) error {
	if s.agent == nil {
		return nil
	}
	mem := s.agent.Memory()
	if mem == nil {
		return nil
	}
	err := mem.AppendEpisodicEvent(ctx, ev, types.TaintNone)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CSVEventLogger.Append", err)
	}
	return nil
}

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

	job.EventLog = sysadminCSVEventLogger{agent: h.Agent}

	res, err := orchestrator.RunCSVFanout(r.Context(), h.Blackboard, job)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
