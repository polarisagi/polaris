package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandleMapReduceRun 触发一条 MapReduce 编排任务。
func (h *SysAdminHandler) HandleMapReduceRun(w http.ResponseWriter, r *http.Request) {
	if h.MapReduceExec == nil {
		httputil.RespondError(w, "MapReduce executor not enabled", nil, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string            `json:"parent_task_id"`
		SubTasks     []types.TaskEntry `json:"sub_tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request format", nil, http.StatusBadRequest)
		return
	}

	res, err := h.MapReduceExec.Execute(r.Context(), req.ParentTaskID, req.SubTasks)
	if err != nil {
		httputil.RespondError(w, "MapReduce task failed", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, map[string]string{"result": string(res)})
}

// HandleParallelRun 触发一条 Parallel 编排任务。
func (h *SysAdminHandler) HandleParallelRun(w http.ResponseWriter, r *http.Request) {
	if h.ParallelExec == nil {
		httputil.RespondError(w, "Parallel executor not enabled", nil, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string            `json:"parent_task_id"`
		SubTasks     []types.TaskEntry `json:"sub_tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request format", nil, http.StatusBadRequest)
		return
	}

	err := h.ParallelExec.Execute(r.Context(), req.ParentTaskID, req.SubTasks)
	if err != nil {
		httputil.RespondError(w, "Parallel task failed", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, map[string]string{"status": "started"})
}

// HandleSequentialRun 触发一条 Sequential 编排任务。
func (h *SysAdminHandler) HandleSequentialRun(w http.ResponseWriter, r *http.Request) {
	if h.SequentialExec == nil {
		httputil.RespondError(w, "Sequential executor not enabled", nil, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string            `json:"parent_task_id"`
		SubTasks     []types.TaskEntry `json:"sub_tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request format", nil, http.StatusBadRequest)
		return
	}

	err := h.SequentialExec.Execute(r.Context(), req.ParentTaskID, req.SubTasks)
	if err != nil {
		httputil.RespondError(w, "Sequential task failed", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, map[string]string{"status": "started"})
}

// HandleSwarmRun 触发一条 Swarm 编排任务。
func (h *SysAdminHandler) HandleSwarmRun(w http.ResponseWriter, r *http.Request) {
	if h.SwarmCoord == nil {
		httputil.RespondError(w, "Swarm coordinator not enabled", nil, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		TaskID      string `json:"task_id"`
		AgentID     string `json:"agent_id"`
		HandoffNote string `json:"handoff_note"`
		Depth       int    `json:"depth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request format", nil, http.StatusBadRequest)
		return
	}

	err := h.SwarmCoord.Handoff(r.Context(), req.TaskID, req.AgentID, req.HandoffNote, req.Depth)
	if err != nil {
		httputil.RespondError(w, "Swarm task failed", err, http.StatusInternalServerError)
		return
	}

	httputil.WriteJSON(w, map[string]string{"status": "handed_off"})
}
