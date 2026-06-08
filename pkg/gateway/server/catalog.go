package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// CatalogProvider sys_providers 字典条目（只读）。
type CatalogProvider struct {
	ID             string          `json:"id"`
	DisplayName    string          `json:"display_name"`
	ProviderType   string          `json:"provider_type"`
	DefaultBaseURL string          `json:"default_base_url"`
	IsLocal        bool            `json:"is_local"`
	DisplayOrder   int             `json:"display_order"`
	Models         []CatalogModel  `json:"models"`
}

// CatalogModel sys_provider_models 字典条目（只读）。
type CatalogModel struct {
	ID             string `json:"id"`
	ModelID        string `json:"model_id"`
	DisplayName    string `json:"display_name"`
	CapabilityTier string `json:"capability_tier"` // smart | fast
	IsReasoning    bool   `json:"is_reasoning"`
	DisplayOrder   int    `json:"display_order"`
}

// handleListCatalogProviders GET /v1/catalog/providers
// 返回内置厂商字典（含各厂商预设模型列表），供前端展示选择器。
func (s *Server) handleListCatalogProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, display_name, provider_type, default_base_url, is_local, display_order
		   FROM sys_providers ORDER BY display_order`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	provMap := make(map[string]*CatalogProvider)
	var order []string
	for rows.Next() {
		p := &CatalogProvider{Models: []CatalogModel{}}
		var isLocal int
		if err := rows.Scan(&p.ID, &p.DisplayName, &p.ProviderType, &p.DefaultBaseURL, &isLocal, &p.DisplayOrder); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.IsLocal = isLocal == 1
		provMap[p.ID] = p
		order = append(order, p.ID)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mrows, err := s.db.QueryContext(r.Context(),
		`SELECT id, catalog_provider_id, model_id, display_name, capability_tier, is_reasoning, display_order
		   FROM sys_provider_models ORDER BY catalog_provider_id, display_order`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer mrows.Close()
	for mrows.Next() {
		m := CatalogModel{}
		var isReasoning int
		var provID string
		if err := mrows.Scan(&m.ID, &provID, &m.ModelID, &m.DisplayName, &m.CapabilityTier, &isReasoning, &m.DisplayOrder); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.IsReasoning = isReasoning == 1
		if p, ok := provMap[provID]; ok {
			p.Models = append(p.Models, m)
		}
	}
	if err := mrows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]*CatalogProvider, 0, len(order))
	for _, id := range order {
		out = append(out, provMap[id])
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"providers": out})
}

// fromCatalogRequest POST /v1/providers/from-catalog 请求体。
type fromCatalogRequest struct {
	CatalogID string `json:"catalog_id"` // sys_providers.id
	APIKey    string `json:"api_key"`
	Name      string `json:"name"`     // 可选，默认取 display_name
	BaseURL   string `json:"base_url"` // 可选，默认取 default_base_url
}

// handleCreateProviderFromCatalog POST /v1/providers/from-catalog
// 用户只需提供 catalog_id + api_key，系统自动：
//  1. 查厂商字典填充 type / base_url
//  2. 从模型字典生成 provider_models，自动分配 default/reasoning/general 角色
//     default   → capability_tier='smart' AND is_reasoning=0，display_order 最小
//     reasoning → is_reasoning=1，display_order 最小
//     general   → 其余全部
func (s *Server) handleCreateProviderFromCatalog(w http.ResponseWriter, r *http.Request) {
	var req fromCatalogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.CatalogID == "" {
		http.Error(w, "catalog_id required", http.StatusBadRequest)
		return
	}

	// 查厂商字典
	var cat CatalogProvider
	var isLocal int
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, display_name, provider_type, default_base_url, is_local
		   FROM sys_providers WHERE id=?`, req.CatalogID,
	).Scan(&cat.ID, &cat.DisplayName, &cat.ProviderType, &cat.DefaultBaseURL, &isLocal)
	if err != nil {
		http.Error(w, "catalog provider not found: "+req.CatalogID, http.StatusNotFound)
		return
	}
	cat.IsLocal = isLocal == 1

	// 非本地厂商必须提供 api_key
	if !cat.IsLocal && req.APIKey == "" {
		http.Error(w, "api_key required for non-local provider", http.StatusBadRequest)
		return
	}

	// 填充可选字段
	name := req.Name
	if name == "" {
		name = cat.DisplayName
	}
	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = cat.DefaultBaseURL
	}

	// 生成 provider ID
	buf := make([]byte, 8)
	rand.Read(buf) //nolint:errcheck
	provID := "prov_" + hex.EncodeToString(buf)
	now := time.Now().UTC().Format(time.RFC3339)

	// 写入 providers
	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO providers(id,name,type,base_url,api_key,project_id,location,sa_key_json,enabled,catalog_id,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,1,?,?,?)`,
		provID, name, cat.ProviderType, baseURL, req.APIKey,
		"", "", "", req.CatalogID, now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 查模型字典
	mrows, err := s.db.QueryContext(r.Context(),
		`SELECT model_id, display_name, capability_tier, is_reasoning
		   FROM sys_provider_models
		  WHERE catalog_provider_id=?
		  ORDER BY display_order`, req.CatalogID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer mrows.Close()

	type catalogModelRow struct {
		modelID     string
		displayName string
		tier        string
		isReasoning bool
	}
	var catalogModels []catalogModelRow
	for mrows.Next() {
		var cm catalogModelRow
		var isR int
		if err := mrows.Scan(&cm.modelID, &cm.displayName, &cm.tier, &isR); err != nil {
			continue
		}
		cm.isReasoning = isR == 1
		catalogModels = append(catalogModels, cm)
	}
	_ = mrows.Close()

	// 角色分配：default(第一个 smart 非推理) → reasoning(第一个推理) → general(其余)
	defaultAssigned := false
	reasoningAssigned := false
	var createdModels []ProviderModel

	for _, cm := range catalogModels {
		role := "general"
		if cm.isReasoning && !reasoningAssigned {
			role = "reasoning"
			reasoningAssigned = true
		} else if !cm.isReasoning && cm.tier == "smart" && !defaultAssigned {
			role = "default"
			defaultAssigned = true
		}

		mbuf := make([]byte, 8)
		rand.Read(mbuf) //nolint:errcheck
		mID := "mdl_" + hex.EncodeToString(mbuf)

		_, err := s.db.ExecContext(r.Context(),
			`INSERT INTO provider_models(id,provider_id,model_id,name,role,enabled,created_at,updated_at)
			 VALUES(?,?,?,?,?,1,?,?)`,
			mID, provID, cm.modelID, cm.displayName, role, now, now)
		if err != nil {
			continue
		}
		createdModels = append(createdModels, ProviderModel{
			ID: mID, ProviderID: provID, ModelID: cm.modelID,
			Name: cm.displayName, Role: role, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		})
	}

	// 若目录无模型（如 Ollama），不报错，返回空模型列表
	if createdModels == nil {
		createdModels = []ProviderModel{}
	}

	s.reloadProviders()

	out := ProviderConfig{
		ID: provID, Name: name, Type: cat.ProviderType,
		BaseURL: baseURL, APIKey: req.APIKey,
		Enabled: true, Models: createdModels,
		CreatedAt: now, UpdatedAt: now,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}
