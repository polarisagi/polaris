package sysadmin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/learning/harnessoverlay"
)

// HandleHarnessHistory 处理 GET /v1/harness/history?limit=...（ADR-0115 WP6）。
func (h *SysAdminHandler) HandleHarnessHistory(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
			limit = l
		}
	}

	overlayDir := filepath.Join(h.DataDir, config.SubdirHarnessOverlay)
	mgr := harnessoverlay.New(overlayDir)

	history, err := mgr.History(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type versionDTO struct {
		Hash      string `json:"hash"`
		Author    string `json:"author"`
		Timestamp string `json:"timestamp"`
		Message   string `json:"message"`
	}
	res := make([]versionDTO, 0, len(history))
	for _, v := range history {
		ts := ""
		if !v.Timestamp.IsZero() {
			ts = v.Timestamp.Format("2006-01-02T15:04:05Z")
		}
		res = append(res, versionDTO{
			Hash:      v.Hash,
			Author:    v.Author,
			Timestamp: ts,
			Message:   v.Message,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"versions": res,
		"count":    len(res),
	}); err != nil {
		slog.Warn("harness: failed to encode history response", "err", err)
	}
}

// HandleHarnessRollback 处理 POST /v1/harness/rollback（ADR-0115 WP6）。
func (h *SysAdminHandler) HandleHarnessRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Version == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}

	overlayDir := filepath.Join(h.DataDir, config.SubdirHarnessOverlay)
	mgr := harnessoverlay.New(overlayDir)

	if err := mgr.Rollback(r.Context(), req.Version); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"restored_to": req.Version,
	}); err != nil {
		slog.Warn("harness: failed to encode rollback response", "err", err)
	}
}
