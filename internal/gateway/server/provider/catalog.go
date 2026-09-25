package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/pkg/types"

	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
)

// CatalogProvider sys_providers 字典条目（只读）。
type CatalogProvider struct {
	ID             string         `json:"id"`
	DisplayName    string         `json:"display_name"`
	ProviderType   string         `json:"provider_type"`
	DefaultBaseURL string         `json:"default_base_url"`
	IsLocal        bool           `json:"is_local"`
	DisplayOrder   int            `json:"display_order"`
	Models         []CatalogModel `json:"models"`
}

// CatalogModel sys_provider_models 字典条目（只读）。
type CatalogModel struct {
	ID              string `json:"id"`
	ModelID         string `json:"model_id"`
	DisplayName     string `json:"display_name"`
	RecommendedRole string `json:"recommended_role"` // default | reasoning（通用由路由层派生，不入字典）
	DisplayOrder    int    `json:"display_order"`
}

// HandleListCatalogProviders GET /v1/catalog/providers
// 返回内置厂商字典（含各厂商预设模型列表），供前端展示选择器。
func (h *ProviderHandler) HandleListCatalogProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT id, display_name, provider_type, default_base_url, is_local, display_order
		   FROM sys_providers ORDER BY display_order`)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	provMap := make(map[string]*CatalogProvider)
	var order []string
	for rows.Next() {
		p := &CatalogProvider{Models: []CatalogModel{}}
		var isLocal int
		if err := rows.Scan(&p.ID, &p.DisplayName, &p.ProviderType, &p.DefaultBaseURL, &isLocal, &p.DisplayOrder); err != nil {
			httputil.RespondError(w, "", err, http.StatusInternalServerError)
			return
		}
		p.IsLocal = isLocal == 1
		provMap[p.ID] = p
		order = append(order, p.ID)
	}
	if err := rows.Err(); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	mrows, err := h.DB.QueryContext(r.Context(),
		`SELECT id, catalog_provider_id, model_id, display_name, recommended_role, display_order
		   FROM sys_provider_models ORDER BY catalog_provider_id, display_order`)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	defer mrows.Close()
	for mrows.Next() {
		m := CatalogModel{}
		var provID string
		if err := mrows.Scan(&m.ID, &provID, &m.ModelID, &m.DisplayName, &m.RecommendedRole, &m.DisplayOrder); err != nil {
			httputil.RespondError(w, "", err, http.StatusInternalServerError)
			return
		}
		if p, ok := provMap[provID]; ok {
			p.Models = append(p.Models, m)
		}
	}
	if err := mrows.Err(); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	out := make([]*CatalogProvider, 0, len(order))
	for _, id := range order {
		out = append(out, provMap[id])
	}

	httputil.WriteJSON(w, map[string]any{"providers": out})
}

// fromCatalogRequest POST /v1/providers/from-catalog 请求体。
type fromCatalogRequest struct {
	CatalogID string `json:"catalog_id"` // sys_providers.id
	APIKey    string `json:"api_key"`
	Name      string `json:"name"`     // 可选，默认取 display_name
	BaseURL   string `json:"base_url"` // 可选，默认取 default_base_url
}

type catalogModelRow struct {
	modelID         string
	displayName     string
	recommendedRole string
}

// HandleCreateProviderFromCatalog POST /v1/providers/from-catalog
// 用户只需提供 catalog_id + api_key，系统自动：
//  1. 查厂商字典填充 type / base_url
//  2. 从模型字典生成 provider_models：对话(default) / 推理(reasoning) 按 recommended_role；
//     通用不落行，由路由层派生为对话模型（internal/llm poolRoles）
//  3. 只补空缺：全局已有启用的 default/reasoning 时，新模型写为 general（备用），不抢占
//
// 厂商与模型同事务写入，任一步失败整体回滚。
func (h *ProviderHandler) HandleCreateProviderFromCatalog(w http.ResponseWriter, r *http.Request) {
	var req fromCatalogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if req.CatalogID == "" {
		http.Error(w, "catalog_id required", http.StatusBadRequest)
		return
	}

	// 查厂商字典
	var cat CatalogProvider
	var isLocal int
	err := h.DB.QueryRowContext(r.Context(),
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

	catalogModels, err := h.fetchCatalogModels(r.Context(), req.CatalogID)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	held, err := h.ProviderRepo.ActiveModelRoles(r.Context())
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	provID := newRecordID("prov_")
	now := time.Now().UTC().Format(time.RFC3339)
	modelRows := assignCatalogRoles(catalogModels, held, provID, now)

	err = h.ProviderRepo.CreateProviderWithModels(r.Context(), types.ProviderRow{
		ID:        provID,
		Name:      name,
		Type:      cat.ProviderType,
		BaseURL:   baseURL,
		APIKey:    req.APIKey,
		Enabled:   true,
		CatalogID: req.CatalogID,
		CreatedAt: now,
		UpdatedAt: now,
	}, modelRows)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	h.reloadProviders()

	// 若目录无模型（如 Ollama），返回空模型列表
	createdModels := make([]ProviderModel, 0, len(modelRows))
	for _, m := range modelRows {
		createdModels = append(createdModels, ProviderModel{
			ID: m.ID, ProviderID: provID, ModelID: m.ModelID,
			Name: m.Name, Role: m.Role, Enabled: m.Enabled,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	out := ProviderConfig{
		ID: provID, Name: name, Type: cat.ProviderType,
		BaseURL: baseURL, APIKey: req.APIKey,
		Enabled: true, Models: createdModels,
		CreatedAt: now, UpdatedAt: now,
	}
	httputil.WriteJSONStatus(w, http.StatusCreated, out)
}

// assignCatalogRoles 把字典推荐角色落为 provider_models 行：default/reasoning 全局独占，
// 已被启用模型持有则新模型降为 general（备用），避免新增厂商静默替换用户在用的对话/推理模型。
func assignCatalogRoles(catalogModels []catalogModelRow, held map[string]bool, provID, now string) []types.ProviderModelRow {
	taken := make(map[string]bool, len(held))
	for role, ok := range held {
		taken[role] = ok
	}
	rows := make([]types.ProviderModelRow, 0, len(catalogModels))
	for _, cm := range catalogModels {
		role := cm.recommendedRole
		if (role != "default" && role != "reasoning") || taken[role] {
			role = "general"
		}
		taken[role] = true
		rows = append(rows, types.ProviderModelRow{
			ID:         newRecordID("mdl_"),
			ProviderID: provID,
			ModelID:    cm.modelID,
			Name:       cm.displayName,
			Role:       role,
			Enabled:    true,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}
	return rows
}

func newRecordID(prefix string) string {
	buf := make([]byte, 8)
	rand.Read(buf) //nolint:errcheck // crypto/rand.Read 自 Go 1.24 起恒返回 nil 错误
	return prefix + hex.EncodeToString(buf)
}

func (h *ProviderHandler) fetchCatalogModels(ctx context.Context, catalogID string) ([]catalogModelRow, error) {
	mrows, err := h.DB.QueryContext(ctx,
		`SELECT model_id, display_name, recommended_role
		   FROM sys_provider_models
		  WHERE catalog_provider_id=?
		  ORDER BY display_order`, catalogID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.fetchCatalogModels", err)
	}
	defer mrows.Close()

	var catalogModels []catalogModelRow
	for mrows.Next() {
		var cm catalogModelRow
		if err := mrows.Scan(&cm.modelID, &cm.displayName, &cm.recommendedRole); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "Server.fetchCatalogModels scan", err)
		}
		catalogModels = append(catalogModels, cm)
	}
	if err := mrows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.fetchCatalogModels rows error", err)
	}
	return catalogModels, nil
}
