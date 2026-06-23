package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// handleTestProvider 取厂商下第一个模型做连通性探测。
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

// ── provider_models CRUD ──────────────────────────────────────────────────────

func (h *ProviderHandler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerID")
	rows, err := h.ProviderRepo.ListModels(r.Context(), providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// ── model-roles ───────────────────────────────────────────────────────────────

// handleGetModelRoles 返回当前 default / reasoning 角色指向的模型信息。
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

// handleSetModelRoles 通过 model id 设置 default / reasoning 角色，原同角色模型重置为 general。
func (h *ProviderHandler) HandleSetModelRoles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModelID   string `json:"default_model_id"`
		ReasoningModelID string `json:"reasoning_model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

// ── probe ─────────────────────────────────────────────────────────────────────

func probeProvider(ctx context.Context, client *http.Client, typ, baseURL, apiKey, modelID, projectID, location string) (bool, string) { //nolint:gocyclo
	switch typ {
	case "openai_compat", "ollama":
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(baseURL, "/")+"/v1/models", nil)
		if err != nil {
			return false, fmt.Sprintf("请求构建失败: %v", err)
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 {
			return false, "API Key 无效（HTTP 401）"
		}
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)

	case "anthropic":
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/v1/models", nil)
		if err != nil {
			return false, fmt.Sprintf("请求构建失败: %v", err)
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		if resp.StatusCode == 401 {
			return false, "API Key 无效（HTTP 401）"
		}
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)

	case "google_agent_platform":
		if apiKey == "" {
			return false, "缺少 API Key"
		}
		model := modelID
		if model == "" {
			model = "gemini-2.0-flash"
		}
		var endpoint string
		if projectID != "" {
			loc := location
			if loc == "" {
				loc = "global"
			}
			var host string
			if loc == "global" {
				host = "https://aiplatform.googleapis.com"
			} else {
				host = "https://" + loc + "-aiplatform.googleapis.com"
			}
			endpoint = fmt.Sprintf(
				"%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent?key=%s",
				host, projectID, loc, model, apiKey)
		} else {
			endpoint = fmt.Sprintf(
				"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
				model, apiKey)
		}
		reqBody := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}],"generationConfig":{"maxOutputTokens":1}}`
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(reqBody))
		if err != nil {
			return false, fmt.Sprintf("请求构建失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		limit := len(raw)
		if limit > 200 {
			limit = 200
		}
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:limit])))
	}
	return false, "未知厂商类型"
}

//nolint:unused
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (h *ProviderHandler) reloadProviders() {
	if h.Registry == nil || h.DB == nil {
		return
	}
	_ = LoadProvidersFromDB(context.Background(), h.DB, h.Registry, h.HTTPClient, h.TBR)
}
