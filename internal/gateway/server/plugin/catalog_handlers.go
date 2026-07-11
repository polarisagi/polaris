package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/authcontext"

	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/extension/marketplace"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

func (h *PluginHandler) HandleListPluginCatalog(w http.ResponseWriter, r *http.Request) {
	installed := h.GetInstalledCatalogIDs(r.Context())
	result := make([]protocol.RegistryEntry, 0)
	result = h.AppendCustomCatalogs(r.Context(), result, installed)
	result = h.AppendCachedCatalogs(r.Context(), result, installed)

	// 去重：同一 ID 只保留第一次出现的条目（已安装版本优先）
	seen := make(map[string]bool)
	uniqueResult := make([]protocol.RegistryEntry, 0, len(result))
	for _, entry := range result {
		if !seen[entry.ID] {
			seen[entry.ID] = true
			uniqueResult = append(uniqueResult, entry)
		} else {
			slog.Warn("polaris-server: found duplicate catalog entry", "id", entry.ID, "name", entry.Name)
		}
	}

	// 排序键：(installed_rank=0/1, marketplace_sort_order, name_lower)
	sort.Slice(uniqueResult, func(i, j int) bool {
		ei, ej := uniqueResult[i], uniqueResult[j]

		// 已安装的排最前
		if ei.Installed != ej.Installed {
			return ei.Installed
		}

		// 同安装状态：官方市场（SortOrder == 0）排在最前，其他的不再按市场细分排序
		isOfficialI := ei.MarketplaceSortOrder == 0
		isOfficialJ := ej.MarketplaceSortOrder == 0
		if isOfficialI != isOfficialJ {
			return isOfficialI
		}

		// 官方内部或其他市场内部，统一按名称排
		return strings.ToLower(ei.Name) < strings.ToLower(ej.Name)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"catalog": uniqueResult,
		"total":   len(uniqueResult),
	})
}

// HandleInstallPlugin 一键安装目录条目。
// POST /v1/plugins/install
func (h *PluginHandler) HandleInstallPlugin(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo,nestif
	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req protocol.PluginInstallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	if req.CatalogID == "" {
		http.Error(w, "catalog_id is required", http.StatusBadRequest)
		return
	}

	// TrustUntrusted(0) 安装直接拒绝
	var trustTier int
	var payload string
	err := h.DB.QueryRowContext(r.Context(),
		`SELECT trust_tier, payload FROM extension_catalog WHERE id=?`, req.CatalogID).
		Scan(&trustTier, &payload)
	if err != nil {
		http.Error(w, "catalog entry not found: "+req.CatalogID, http.StatusNotFound)
		return
	}
	if trustTier == 0 {
		http.Error(w, "untrusted entry, installation rejected", http.StatusForbidden)
		return
	}

	var entry protocol.RegistryEntry
	if err := json.Unmarshal([]byte(payload), &entry); err != nil {
		http.Error(w, "malformed catalog entry", http.StatusInternalServerError)
		return
	}

	// 防重复
	var existCount int
	errExist := h.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM extension_instances WHERE catalog_id=?`, req.CatalogID).
		Scan(&existCount)
	if errExist != nil {
		http.Error(w, "failed to check existing installation", http.StatusInternalServerError)
		return
	}
	if existCount > 0 {
		http.Error(w, "already installed", http.StatusConflict)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	extID := util.GenerateHumanReadableID("ext", entry.Name)

	// PolicyGate 是安全门，不允许 nil 跳过（fail-closed）。
	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx := authcontext.FromContext(r.Context())
	principal := authCtx.UserID
	if principal == "" {
		principal = "user"
	}
	// plugin 类型在下载前无法确认是否含 hooks；
	// trust_tier < 3 (Community 及以下) 保守假设有 hooks，强制走 HITL 审查。
	hasHooks := entry.Type == "plugin" && entry.TrustTier < 3
	installReq := protocol.ExtensionInstallRequest{
		Principal:   principal,
		ExtensionID: extID,
		ExtType:     entry.Type,
		TrustTier:   entry.TrustTier,
		Publisher:   entry.Publisher,
		HasHooks:    hasHooks,
	}
	if err := h.InstallMgr.Authorize(r.Context(), installReq); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			// Trigger HITL via hitlGateway
			if h.HITLGateway != nil {
				bgCtx, cancel := context.WithTimeout(protocol.Detach(r.Context()), 30*time.Minute)
				concurrent.SafeGo(bgCtx, "gateway.plugin.hitl_install_catalog_entry", func(bgCtx context.Context) {
					defer cancel()
					resp, err := h.HITLGateway.Prompt(bgCtx, apptypes.HITLPrompt{
						ID:             extID,
						CheckpointType: "security_review",
						PromptText:     "Approve installation for extension: " + entry.Name,
						Options: []apptypes.HITLOption{
							{Key: "approve", Label: "Approve"},
							{Key: "deny", Label: "Deny"},
						},
					})
					if err == nil && resp != nil && resp.Approved {
						switch entry.Type {
						case "mcp", "":
							_, err = h.internalInstallMCP(bgCtx, extID, &entry, req, now, true)
						default: // skill | plugin | app
							_, err = h.internalInstallGeneric(bgCtx, extID, &entry, req, now, true)
						}
						if err != nil {
							slog.Error("marketplace: failed to install extension after HITL", "id", extID, "err", err)
						} else {
							h.ClearToolSchemaCache()
							slog.Info("marketplace: installed extension via HITL", "id", extID, "type", entry.Type)
						}
					}
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted) // 202 Accepted
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		httputil.RespondError(w, "Internal Server Error", err, http.StatusForbidden)
		return
	}

	switch entry.Type {
	case "mcp", "":
		h.installMCPExtension(w, r, extID, &entry, req, now)
	default: // skill | plugin | app
		h.installGenericExtension(w, r, extID, &entry, req, now)
	}
	h.ClearToolSchemaCache()
	slog.Info("marketplace: installed extension", "id", extID, "type", entry.Type, "name", entry.Name)
}

// HandleUninstallPlugin 卸载扩展（通过 catalog_id 定位 extension_instances）。
// DELETE /v1/plugins/{catalogID}
func (h *PluginHandler) HandleUninstallPlugin(w http.ResponseWriter, r *http.Request) {
	catalogID := r.PathValue("catalogID")

	if h.InstallMgr == nil {
		http.Error(w, "marketplace manager not initialized", http.StatusInternalServerError)
		return
	}

	err := h.InstallMgr.UninstallExtension(r.Context(), catalogID)
	if err != nil {
		if strings.Contains(err.Error(), "not installed") {
			httputil.RespondError(w, "Internal Server Error", err, http.StatusNotFound)
		} else {
			httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		}
		return
	}
	h.ClearToolSchemaCache()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "uninstalled"}) //nolint:errcheck
}

// Marketplace CRUD ---------------------------------------------------------

// HandleListMarketplaces GET /v1/plugins/marketplaces
func (h *PluginHandler) HandleListMarketplaces(w http.ResponseWriter, r *http.Request) {
	var mps []protocol.Marketplace
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at
		 FROM plugin_marketplaces
		 ORDER BY sort_order ASC, created_at ASC`)
	if err == nil {
		for rows.Next() {
			var m protocol.Marketplace
			if rows.Scan(&m.ID, &m.Name, &m.Type, &m.Publisher, &m.RepoURL,
				&m.Description, &m.IsBuiltin, &m.TrustTier, &m.Enabled, &m.SortOrder, &m.CreatedAt) == nil {
				mps = append(mps, m)
			}
		}
		rows.Close()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"marketplaces": mps, "total": len(mps)})
}

// HandleAddMarketplace POST /v1/plugins/marketplaces
func (h *PluginHandler) HandleAddMarketplace(w http.ResponseWriter, r *http.Request) {
	var req protocol.Marketplace
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	req.ID = util.GenerateHumanReadableID("mp", req.Name)
	req.IsBuiltin = 0
	req.TrustTier = 2 // Community
	req.Enabled = 1
	req.CreatedAt = now

	// 新增市场排在所有现有市场之后：取当前最大 sort_order + 10，留出调整空间
	maxOrder, _ := h.ExtRepo.GetMaxMarketplaceSortOrder(r.Context())
	req.SortOrder = maxOrder + 10

	err := h.ExtRepo.CreateMarketplace(r.Context(), req)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(req)
}

// HandleDeleteMarketplace DELETE /v1/plugins/marketplaces/{id}
func (h *PluginHandler) HandleDeleteMarketplace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deleted, err := h.ExtRepo.DeleteMarketplace(r.Context(), id)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "marketplace not found or is builtin", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// cond 三元运算辅助（Go 无三元）
func cond(pred bool, a, b string) string {
	if pred {
		return a
	}
	return b
}

// downloadAndInstallExtension 异步下载并安装扩展，更新数据库。
//
