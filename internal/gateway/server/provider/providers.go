package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ProviderConfig LLM 厂商凭据（不含具体模型，模型由 ProviderModel 管理）。
type ProviderConfig struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"` // openai_compat | anthropic | google_agent_platform | ollama
	BaseURL   string          `json:"base_url"`
	APIKey    string          `json:"api_key"`
	ProjectID string          `json:"project_id"` // Google Agent Platform
	Location  string          `json:"location"`   // Google Agent Platform region
	SAKeyJSON string          `json:"sa_key_json"`
	Enabled   bool            `json:"enabled"`
	Models    []ProviderModel `json:"models"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

// ProviderModel 厂商下的具体模型条目，携带路由角色。
type ProviderModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Name       string `json:"name"`
	Role       string `json:"role"` // general | default | reasoning
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// ── providers CRUD ────────────────────────────────────────────────────────────

func (h *ProviderHandler) listProviders(db protocol.SQLQuerier) ([]*ProviderConfig, error) {
	rows, err := db.QueryContext(context.Background(),
		`SELECT id,name,type,base_url,api_key,project_id,location,sa_key_json,enabled,created_at,updated_at
		   FROM providers ORDER BY created_at`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.listProviders", err)
	}
	defer rows.Close()

	provMap := make(map[string]*ProviderConfig)
	var order []string
	for rows.Next() {
		p := &ProviderConfig{Models: []ProviderModel{}}
		var enabled int
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKey,
			&p.ProjectID, &p.Location, &p.SAKeyJSON, &enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "Server.listProviders", err)
		}
		p.Enabled = enabled == 1
		provMap[p.ID] = p
		order = append(order, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.listProviders", err)
	}

	mrows, err := db.QueryContext(context.Background(),
		`SELECT id,provider_id,model_id,name,role,enabled,created_at,updated_at
		   FROM provider_models ORDER BY created_at`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.listProviders", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		m := ProviderModel{}
		var enabled int
		if err := mrows.Scan(&m.ID, &m.ProviderID, &m.ModelID, &m.Name, &m.Role, &enabled, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "Server.listProviders", err)
		}
		m.Enabled = enabled == 1
		if p, ok := provMap[m.ProviderID]; ok {
			p.Models = append(p.Models, m)
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.listProviders", err)
	}

	out := make([]*ProviderConfig, 0, len(order))
	for _, id := range order {
		out = append(out, provMap[id])
	}
	return out, nil
}

func (h *ProviderHandler) HandleListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := h.listProviders(h.DB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*ProviderConfig{}
	}
	// [P1修复] API key / SA 密钥明文不得随列表接口下发给前端。
	// 前端只需判断 api_key 是否已设置（非空），不需要知道具体值。
	// 脱敏规则：已设置 → 返回固定掩码字符串，未设置 → 返回空字符串。
	for _, p := range list {
		if p.APIKey != "" {
			p.APIKey = "••••••••"
		}
		if p.SAKeyJSON != "" {
			p.SAKeyJSON = "••••••••"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": list})
}

func (h *ProviderHandler) HandleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var p ProviderConfig
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if p.ID == "" {
		b := make([]byte, 8)
		rand.Read(b) //nolint:errcheck
		p.ID = "prov_" + hex.EncodeToString(b)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err := h.ProviderRepo.CreateProvider(r.Context(), types.ProviderRow{
		ID:        p.ID,
		Name:      p.Name,
		Type:      p.Type,
		BaseURL:   p.BaseURL,
		APIKey:    p.APIKey,
		ProjectID: p.ProjectID,
		Location:  p.Location,
		SAKeyJSON: p.SAKeyJSON,
		Enabled:   p.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.Models = []ProviderModel{}
	p.CreatedAt, p.UpdatedAt = now, now
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(p)
}

func (h *ProviderHandler) HandleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("providerID")
	var p ProviderConfig
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.ID = id
	now := time.Now().UTC().Format(time.RFC3339)
	err := h.ProviderRepo.UpdateProvider(r.Context(), id, types.ProviderRow{
		Name:      p.Name,
		Type:      p.Type,
		BaseURL:   p.BaseURL,
		APIKey:    p.APIKey,
		ProjectID: p.ProjectID,
		Location:  p.Location,
		SAKeyJSON: p.SAKeyJSON,
		Enabled:   p.Enabled,
		UpdatedAt: now,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.UpdatedAt = now
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (h *ProviderHandler) HandleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("providerID")
	err := h.ProviderRepo.DeleteProvider(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// HandleTestProvider 取厂商下第一个模型做连通性探测。
func (h *ProviderHandler) HandleTestProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("providerID")
	p, err := h.ProviderRepo.GetProvider(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var modelID string
	if models, err := h.ProviderRepo.ListModels(r.Context(), id); err == nil && len(models) > 0 {
		modelID = models[0].ModelID
	}

	ok, msg := probeProvider(r.Context(), h.HTTPClient, p.Type, p.BaseURL, p.APIKey, modelID, p.ProjectID, p.Location)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "message": msg})
}

// provider_models CRUD + model-roles 见 providers_models.go（R7 拆分）。
// 厂商连通性探测(probeProvider) + boolToInt + reloadProviders 见 providers_probe.go（R7 拆分）。
