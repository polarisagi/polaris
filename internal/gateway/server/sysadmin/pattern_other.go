package sysadmin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/pkg/types"
)

// HandleMapReduceRun 触发一条 MapReduce 编排任务。
func (h *SysAdminHandler) HandleMapReduceRun(w http.ResponseWriter, r *http.Request) {
	if h.MapReduceExec == nil {
		http.Error(w, "MapReduce executor not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string            `json:"parent_task_id"`
		SubTasks     []types.TaskEntry `json:"sub_tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request format", http.StatusBadRequest)
		return
	}

	res, err := h.MapReduceExec.Execute(context.Background(), req.ParentTaskID, req.SubTasks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": string(res)})
}

// HandleParallelRun 触发一条 Parallel 编排任务。
func (h *SysAdminHandler) HandleParallelRun(w http.ResponseWriter, r *http.Request) {
	if h.ParallelExec == nil {
		http.Error(w, "Parallel executor not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string            `json:"parent_task_id"`
		SubTasks     []types.TaskEntry `json:"sub_tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request format", http.StatusBadRequest)
		return
	}

	err := h.ParallelExec.Execute(context.Background(), req.ParentTaskID, req.SubTasks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// HandleSequentialRun 触发一条 Sequential 编排任务。
func (h *SysAdminHandler) HandleSequentialRun(w http.ResponseWriter, r *http.Request) {
	if h.SequentialExec == nil {
		http.Error(w, "Sequential executor not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		ParentTaskID string            `json:"parent_task_id"`
		SubTasks     []types.TaskEntry `json:"sub_tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request format", http.StatusBadRequest)
		return
	}

	err := h.SequentialExec.Execute(context.Background(), req.ParentTaskID, req.SubTasks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// HandleSwarmRun 触发一条 Swarm 编排任务。
func (h *SysAdminHandler) HandleSwarmRun(w http.ResponseWriter, r *http.Request) {
	if h.SwarmCoord == nil {
		http.Error(w, "Swarm coordinator not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		TaskID      string `json:"task_id"`
		AgentID     string `json:"agent_id"`
		HandoffNote string `json:"handoff_note"`
		Depth       int    `json:"depth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request format", http.StatusBadRequest)
		return
	}

	err := h.SwarmCoord.Handoff(context.Background(), req.TaskID, req.AgentID, req.HandoffNote, req.Depth)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "handed_off"})
}
