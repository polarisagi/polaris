package sysadmin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/checkpoint"
)

// HandleListCheckpoints 处理 GET /v1/checkpoints?project_dir=...&limit=...（ADR-0115 WP1）。
func (h *SysAdminHandler) HandleListCheckpoints(w http.ResponseWriter, r *http.Request) {
	projectDir := r.URL.Query().Get("project_dir")
	if projectDir == "" {
		projectDir = h.WorkspaceDir
	}
	limit := 20
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
			limit = l
		}
	}

	retentionCount := config.CurrentThresholds().M4Kernel.CheckpointRetentionCount
	store, err := checkpoint.NewStore(h.DataDir, projectDir, retentionCount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	snaps, err := store.List(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type snapDTO struct {
		Hash      string `json:"hash"`
		Message   string `json:"message"`
		Timestamp string `json:"timestamp"`
	}
	res := make([]snapDTO, 0, len(snaps))
	for _, s := range snaps {
		ts := ""
		if !s.Timestamp.IsZero() {
			ts = s.Timestamp.Format("2006-01-02T15:04:05Z")
		}
		res = append(res, snapDTO{
			Hash:      s.Hash,
			Message:   s.Message,
			Timestamp: ts,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"project_dir": projectDir,
		"snapshots":   res,
		"count":       len(res),
	}); err != nil {
		slog.Warn("checkpoint: failed to encode list response", "err", err)
	}
}

// HandleRestoreCheckpoint 处理 POST /v1/checkpoints/restore（ADR-0115 WP1）。
func (h *SysAdminHandler) HandleRestoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectDir string `json:"project_dir"`
		Hash       string `json:"hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Hash == "" {
		http.Error(w, "hash is required", http.StatusBadRequest)
		return
	}
	if req.ProjectDir == "" {
		req.ProjectDir = h.WorkspaceDir
	}

	retentionCount := config.CurrentThresholds().M4Kernel.CheckpointRetentionCount
	store, err := checkpoint.NewStore(h.DataDir, req.ProjectDir, retentionCount)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := store.Restore(r.Context(), req.Hash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"restored_to": req.Hash,
		"project_dir": req.ProjectDir,
		"message":     "checkpoint restored successfully",
	}); err != nil {
		slog.Warn("checkpoint: failed to encode restore response", "err", err)
	}
}
