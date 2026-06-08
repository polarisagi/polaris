package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

func (s *Server) listProviders(db *sql.DB) ([]*ProviderConfig, error) {
	rows, err := db.Query(
		`SELECT id,name,type,base_url,api_key,project_id,location,sa_key_json,enabled,created_at,updated_at
		   FROM providers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	provMap := make(map[string]*ProviderConfig)
	var order []string
	for rows.Next() {
		p := &ProviderConfig{Models: []ProviderModel{}}
		var enabled int
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKey,
			&p.ProjectID, &p.Location, &p.SAKeyJSON, &enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Enabled = enabled == 1
		provMap[p.ID] = p
		order = append(order, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mrows, err := db.Query(
		`SELECT id,provider_id,model_id,name,role,enabled,created_at,updated_at
		   FROM provider_models ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		m := ProviderModel{}
		var enabled int
		if err := mrows.Scan(&m.ID, &m.ProviderID, &m.ModelID, &m.Name, &m.Role, &enabled, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Enabled = enabled == 1
		if p, ok := provMap[m.ProviderID]; ok {
			p.Models = append(p.Models, m)
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	out := make([]*ProviderConfig, 0, len(order))
	for _, id := range order {
		out = append(out, provMap[id])
	}
	return out, nil
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := s.listProviders(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []*ProviderConfig{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": list})
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
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
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO providers(id,name,type,base_url,api_key,project_id,location,sa_key_json,enabled,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Type, p.BaseURL, p.APIKey,
		p.ProjectID, p.Location, p.SAKeyJSON, boolToInt(p.Enabled), now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.Models = []ProviderModel{}
	p.CreatedAt, p.UpdatedAt = now, now
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("providerID")
	var p ProviderConfig
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.ID = id
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE providers SET name=?,type=?,base_url=?,api_key=?,project_id=?,location=?,sa_key_json=?,enabled=?,updated_at=?
		 WHERE id=?`,
		p.Name, p.Type, p.BaseURL, p.APIKey,
		p.ProjectID, p.Location, p.SAKeyJSON, boolToInt(p.Enabled), now, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p.UpdatedAt = now
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("providerID")
	_, err := s.db.ExecContext(r.Context(), `DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// handleTestProvider 取厂商下第一个模型做连通性探测。
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("providerID")
	row := s.db.QueryRowContext(r.Context(),
		`SELECT type,base_url,api_key,project_id,location FROM providers WHERE id=?`, id)
	var typ, baseURL, apiKey, projectID, location string
	if err := row.Scan(&typ, &baseURL, &apiKey, &projectID, &location); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var modelID string
	s.db.QueryRowContext(r.Context(),
		`SELECT model_id FROM provider_models WHERE provider_id=? ORDER BY created_at LIMIT 1`, id,
	).Scan(&modelID) //nolint:errcheck

	ok, msg := probeProvider(typ, baseURL, apiKey, modelID, projectID, location)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "message": msg})
}

// ── provider_models CRUD ──────────────────────────────────────────────────────

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerID")
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id,provider_id,model_id,name,role,enabled,created_at,updated_at
		   FROM provider_models WHERE provider_id=? ORDER BY created_at`, providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	models := []ProviderModel{}
	for rows.Next() {
		m := ProviderModel{}
		var enabled int
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.ModelID, &m.Name, &m.Role, &enabled, &m.CreatedAt, &m.UpdatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.Enabled = enabled == 1
		models = append(models, m)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
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
		s.db.ExecContext(r.Context(), `UPDATE provider_models SET role='general' WHERE role=?`, m.Role) //nolint:errcheck
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO provider_models(id,provider_id,model_id,name,role,enabled,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		m.ID, m.ProviderID, m.ModelID, m.Name, m.Role, boolToInt(m.Enabled), now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.CreatedAt, m.UpdatedAt = now, now
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(m)
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
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
		s.db.ExecContext(r.Context(), `UPDATE provider_models SET role='general' WHERE role=? AND id!=?`, m.Role, modelID) //nolint:errcheck
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE provider_models SET model_id=?,name=?,role=?,enabled=?,updated_at=?
		 WHERE id=? AND provider_id=?`,
		m.ModelID, m.Name, m.Role, boolToInt(m.Enabled), now, modelID, providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	m.UpdatedAt = now
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerID")
	modelID := r.PathValue("modelID")
	_, err := s.db.ExecContext(r.Context(),
		`DELETE FROM provider_models WHERE id=? AND provider_id=?`, modelID, providerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// ── model-roles ───────────────────────────────────────────────────────────────

// handleGetModelRoles 返回当前 default / reasoning 角色指向的模型信息。
func (s *Server) handleGetModelRoles(w http.ResponseWriter, r *http.Request) {
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
	s.db.QueryRowContext(r.Context(), query, "default").
		Scan(&def.ModelID, &def.ModelName, &def.ProviderID, &def.ProviderName) //nolint:errcheck
	s.db.QueryRowContext(r.Context(), query, "reasoning").
		Scan(&reasoning.ModelID, &reasoning.ModelName, &reasoning.ProviderID, &reasoning.ProviderName) //nolint:errcheck

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"default":   def,
		"reasoning": reasoning,
	})
}

// handleSetModelRoles 通过 model id 设置 default / reasoning 角色，原同角色模型重置为 general。
func (s *Server) handleSetModelRoles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModelID   string `json:"default_model_id"`
		ReasoningModelID string `json:"reasoning_model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.db.ExecContext(r.Context(), `UPDATE provider_models SET role='general' WHERE role IN ('default','reasoning')`) //nolint:errcheck
	if req.DefaultModelID != "" {
		s.db.ExecContext(r.Context(), `UPDATE provider_models SET role='default' WHERE id=?`, req.DefaultModelID) //nolint:errcheck
	}
	if req.ReasoningModelID != "" {
		s.db.ExecContext(r.Context(), `UPDATE provider_models SET role='reasoning' WHERE id=?`, req.ReasoningModelID) //nolint:errcheck
	}
	s.reloadProviders()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ── probe ─────────────────────────────────────────────────────────────────────

func probeProvider(typ, baseURL, apiKey, modelID, projectID, location string) (bool, string) { //nolint:gocyclo
	client := &http.Client{Timeout: 10 * time.Second}

	switch typ {
	case "openai_compat", "ollama":
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		req, _ := http.NewRequest("GET", strings.TrimRight(baseURL, "/")+"/v1/models", nil)
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
		req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
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
		req, _ := http.NewRequest("POST", endpoint, bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("连接失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			return true, "连接正常"
		}
		raw, _ := io.ReadAll(resp.Body)
		limit := len(raw)
		if limit > 200 {
			limit = 200
		}
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:limit])))
	}
	return false, "未知厂商类型"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) reloadProviders() {
	if s.registry == nil || s.db == nil {
		return
	}
	_ = LoadProvidersFromDB(context.Background(), s.db, s.registry, s.httpClient)
}
