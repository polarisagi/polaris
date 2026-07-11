package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// provider_models CRUD + model-roles（R7 拆分自 providers.go）。
// ProviderConfig/ProviderModel 类型定义 + providers CRUD 见 providers.go；
// 厂商连通性探测(probeProvider)见 providers_probe.go。
// ============================================================================

// ── provider_models CRUD ──────────────────────────────────────────────────────

func (h *ProviderHandler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerID")
	rows, err := h.ProviderRepo.ListModels(r.Context(), providerID)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	models := make([]ProviderModel, 0, len(rows))
	for _, row := range rows {
		models = append(models, ProviderModel{
			ID:         row.ID,
			ProviderID: row.ProviderID,
			ModelID:    row.ModelID,
			Name:       row.Name,
			Role:       row.Role,
			Enabled:    row.Enabled,
			CreatedAt:  row.CreatedAt,
			UpdatedAt:  row.UpdatedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func (h *ProviderHandler) HandleCreateModel(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerID")
	var m ProviderModel
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	m.ProviderID = providerID
	if m.ID == "" {
		b := make([]byte, 8)
		rand.Read(b) //nolint:errcheck
		m.ID = "mdl_" + hex.EncodeToString(b)
	}
	if m.Role == "" {
		m.Role = "general"
	}
	if m.Name == "" {
		m.Name = m.ModelID
	}
	now := time.Now().UTC().Format(time.RFC3339)
	// 独占角色：同角色只能有一个 default/reasoning
	if m.Role == "default" || m.Role == "reasoning" {
		_ = h.ProviderRepo.ClearModelRoles(r.Context(), []string{m.Role}, "")
	}
	err := h.ProviderRepo.UpsertModel(r.Context(), types.ProviderModelRow{
		ID:         m.ID,
		ProviderID: m.ProviderID,
		ModelID:    m.ModelID,
		Name:       m.Name,
		Role:       m.Role,
		Enabled:    m.Enabled,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	m.CreatedAt, m.UpdatedAt = now, now
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(m)
}

func (h *ProviderHandler) HandleUpdateModel(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerID")
	modelID := r.PathValue("modelID")
	var m ProviderModel
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	m.ID = modelID
	m.ProviderID = providerID
	if m.Role == "" {
		m.Role = "general"
	}
	if m.Name == "" {
		m.Name = m.ModelID
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if m.Role == "default" || m.Role == "reasoning" {
		_ = h.ProviderRepo.ClearModelRoles(r.Context(), []string{m.Role}, modelID)
	}
	err := h.ProviderRepo.UpsertModel(r.Context(), types.ProviderModelRow{
		ID:         modelID,
		ProviderID: providerID,
		ModelID:    m.ModelID,
		Name:       m.Name,
		Role:       m.Role,
		Enabled:    m.Enabled,
		UpdatedAt:  now,
	})
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	m.UpdatedAt = now
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

func (h *ProviderHandler) HandleDeleteModel(w http.ResponseWriter, r *http.Request) {
	_ = r.PathValue("providerID")
	modelID := r.PathValue("modelID")
	err := h.ProviderRepo.DeleteModel(r.Context(), modelID)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// ── model-roles ───────────────────────────────────────────────────────────────

// HandleGetModelRoles 返回当前 default / reasoning 角色指向的模型信息。
func (h *ProviderHandler) HandleGetModelRoles(w http.ResponseWriter, r *http.Request) {
	type roleEntry struct {
		ModelID      string `json:"model_id"`
		ModelName    string `json:"model_name"`
		ProviderID   string `json:"provider_id"`
		ProviderName string `json:"provider_name"`
	}
	query := `SELECT m.id, COALESCE(NULLIF(m.name,''), m.model_id), p.id, p.name
	            FROM provider_models m JOIN providers p ON p.id=m.provider_id
	           WHERE m.role=? AND m.enabled=1 AND p.enabled=1
	           ORDER BY m.updated_at DESC LIMIT 1`
	var def, reasoning roleEntry
	h.DB.QueryRowContext(r.Context(), query, "default").
		Scan(&def.ModelID, &def.ModelName, &def.ProviderID, &def.ProviderName) //nolint:errcheck
	h.DB.QueryRowContext(r.Context(), query, "reasoning").
		Scan(&reasoning.ModelID, &reasoning.ModelName, &reasoning.ProviderID, &reasoning.ProviderName) //nolint:errcheck

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"default":   def,
		"reasoning": reasoning,
	})
}

// HandleSetModelRoles 通过 model id 设置 default / reasoning 角色，原同角色模型重置为 general。
func (h *ProviderHandler) HandleSetModelRoles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModelID   string `json:"default_model_id"`
		ReasoningModelID string `json:"reasoning_model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	_ = h.ProviderRepo.ClearModelRoles(r.Context(), []string{"default", "reasoning"}, "")

	if req.DefaultModelID != "" {
		_ = h.ProviderRepo.SetModelRole(r.Context(), req.DefaultModelID, "default")
	}
	if req.ReasoningModelID != "" {
		_ = h.ProviderRepo.SetModelRole(r.Context(), req.ReasoningModelID, "reasoning")
	}
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
