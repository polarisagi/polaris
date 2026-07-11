package sysadmin

import (
	"encoding/json"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/protocol"
)

func (h *SysAdminHandler) HandleListApps(w http.ResponseWriter, r *http.Request) {
	enabledOnly := r.URL.Query().Get("enabled") == "true"
	apps, err := h.AppRepo.ListApps(r.Context(), enabledOnly)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	if apps == nil {
		apps = []*protocol.App{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apps)
}

func (h *SysAdminHandler) HandleCreateApp(w http.ResponseWriter, r *http.Request) {
	var app protocol.App
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if app.ID == "" {
		http.Error(w, "app ID is required", http.StatusBadRequest)
		return
	}
	if err := h.AppRepo.CreateApp(r.Context(), &app); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(app)
}

func (h *SysAdminHandler) HandleGetApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, err := h.AppRepo.GetApp(r.Context(), id)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(app)
}

func (h *SysAdminHandler) HandleUpdateApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var app protocol.App
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	app.ID = id
	if err := h.AppRepo.UpdateApp(r.Context(), &app); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(app)
}

func (h *SysAdminHandler) HandleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.AppRepo.DeleteApp(r.Context(), id); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *SysAdminHandler) HandleSetAppEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if err := h.AppRepo.SetAppEnabled(r.Context(), id, req.Enabled); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
