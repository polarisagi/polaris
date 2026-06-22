package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"

	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/extension/marketplace"

	"github.com/polarisagi/polaris/internal/protocol"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// GetInstalledCatalogIDs 返回所有已安装的 catalog_id 到 installed_version 的映射。
// SSoT：仅查 extension_instances，不再 UNION 多表。
func (h *PluginHandler) GetInstalledCatalogIDs(ctx context.Context) map[string]string {
	installed := map[string]string{}
	rows, err := h.DB.QueryContext(ctx,
		`SELECT catalog_id, config FROM extension_instances WHERE catalog_id != ''`)
	if err != nil {
		return installed
	}
	defer rows.Close()
	for rows.Next() {
		var cid, configStr string
		if rows.Scan(&cid, &configStr) == nil {
			version := ""
			var cfg map[string]any
			if json.Unmarshal([]byte(configStr), &cfg) == nil {
				if v, ok := cfg["version"].(string); ok {
					version = v
				}
			}
			installed[cid] = version
		}
	}
	return installed
}

// AppendCustomCatalogs 追加用户自建扩展（origin=user）到目录列表。
// 全走 extension_instances，不再散查 skills/plugins/apps 三表。
func (h *PluginHandler) AppendCustomCatalogs(ctx context.Context, result []protocol.RegistryEntry, _ map[string]string) []protocol.RegistryEntry {
	rows, err := h.DB.QueryContext(ctx,
		`SELECT id, ext_type, name, publisher, trust_tier, config
		 FROM extension_instances
		 WHERE origin = 'user' AND status = 'installed'`)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var e protocol.RegistryEntry
		var configJSON string
		if err := rows.Scan(&e.ID, &e.Type, &e.Name, &e.Publisher, &e.TrustTier, &configJSON); err != nil {
			continue
		}
		e.Installed = true
		// 从 config JSON 提取 URL（app）/ command（mcp）等展示字段，容错忽略
		var cfg map[string]any
		if json.Unmarshal([]byte(configJSON), &cfg) == nil {
			if v, ok := cfg["url"].(string); ok {
				e.URL = v
			}
			if v, ok := cfg["command"].(string); ok {
				e.Command = v
			}
		}
		e.MarketplaceSortOrder = 1000 // 用户自定义的扩展放到同类状态的最后
		result = append(result, e)
	}
	return result
}

// AppendCachedCatalogs 追加市场同步缓存条目，叠加安装状态。
func (h *PluginHandler) AppendCachedCatalogs(ctx context.Context, result []protocol.RegistryEntry, installed map[string]string) []protocol.RegistryEntry {
	rows, err := h.DB.QueryContext(ctx, `
		SELECT c.payload, COALESCE(m.sort_order, 999) 
		FROM extension_catalog c 
		LEFT JOIN plugin_marketplaces m ON c.marketplace_id = m.id`)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var payload string
		var sortOrder int
		if err := rows.Scan(&payload, &sortOrder); err != nil {
			continue
		}
		var entry protocol.RegistryEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			continue
		}
		if ver, ok := installed[entry.ID]; ok {
			entry.Installed = true
			entry.InstalledVersion = ver
		} else {
			entry.Installed = false
		}
		entry.MarketplaceSortOrder = sortOrder
		result = append(result, entry)
	}
	return result
}

// HandleListPluginCatalog 返回扩展目录列表（用户自建 + 市场缓存）。
// 排序规则：已安装优先 → 官方市场优先（SortOrder == 0）→ 名字字母序。
// 已安装的条目只出现一次（installed=true），不在未安装区重复展示。
// GET /v1/plugins/catalog
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
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	h.DB.QueryRowContext(r.Context(), //nolint:errcheck
		`SELECT COUNT(*) FROM extension_instances WHERE catalog_id=?`, req.CatalogID).
		Scan(&existCount) //nolint:errcheck
	if existCount > 0 {
		http.Error(w, "already installed", http.StatusConflict)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	extID := "ext_" + hex.EncodeToString(b)

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
	installReq := marketplace.InstallRequest{
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
				go func() {
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
							_, _ = h.internalInstallMCP(bgCtx, extID, &entry, req, now)
						default: // skill | plugin | app
							_, _ = h.internalInstallGeneric(bgCtx, extID, &entry, req, now)
						}
						h.ClearToolSchemaCache()
						slog.Info("marketplace: installed extension via HITL", "id", extID, "type", entry.Type)
					}
				}()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted) // 202 Accepted
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
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

// installMCPExtension 安装 MCP 类型：写 extension_instances + mcp_servers + 异步启动。
func (h *PluginHandler) internalInstallMCP(ctx context.Context, extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) (any, error) {
	cfg := types.MCPServerConfig{
		Transport: entry.Transport,
		Command:   entry.Command,
		Args:      entry.Args,
		Env:       entry.Env,
		URL:       entry.URL,
		Timeout:   entry.Timeout,
		TrustTier: entry.TrustTier,
		Enabled:   true,
	}
	cfg.Name = cond(req.Name != "", req.Name, entry.Name)
	if len(req.Args) > 0 {
		cfg.Args = req.Args
	}
	if len(req.Env) > 0 {
		merged := make(map[string]string, len(cfg.Env)+len(req.Env))
		maps.Copy(merged, cfg.Env)
		maps.Copy(merged, req.Env)
		cfg.Env = merged
	}
	if req.URL != "" {
		cfg.URL = req.URL
	}
	if req.Timeout > 0 {
		cfg.Timeout = req.Timeout
	}

	mcpID := "mcp_" + extID[4:]
	cfg.ID = mcpID

	argsBytes, _ := json.Marshal(cfg.Args)
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	envBytes, _ := json.Marshal(cfg.Env)

	configMap := map[string]any{}
	if entry.Version != "" {
		configMap["version"] = entry.Version
	}
	configJSON, _ := json.Marshal(configMap)

	installReq := marketplace.InstallRequest{
		Principal:   "system", // Auth is already checked in HandleInstallPlugin
		ExtensionID: extID,
		CatalogID:   req.CatalogID,
		Name:        cfg.Name,
		ExtType:     "mcp",
		TrustTier:   entry.TrustTier,
		Publisher:   entry.Publisher,
		Config:      string(configJSON),
		RuntimeID:   mcpID,
	}

	if err := h.InstallMgr.InstallExtension(ctx, installReq); err != nil {
		return nil, fmt.Errorf("Server.internalInstallMCP: %w", err)
	}

	err := h.ExtRepo.UpsertMCPServer(ctx, apptypes.MCPServerRow{
		ID:        mcpID,
		Name:      cfg.Name,
		Transport: cfg.Transport,
		Command:   cfg.Command,
		Args:      string(argsBytes),
		Env:       string(envBytes),
		URL:       cfg.URL,
		Enabled:   true,
		Timeout:   cfg.Timeout,
		TrustTier: cfg.TrustTier,
		CatalogID: req.CatalogID,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		_ = h.ExtRepo.DeleteInstance(ctx, extID)
		return nil, fmt.Errorf("Server.internalInstallMCP: %w", err)
	}

	if h.MCPMgr != nil {
		//nolint:errcheck
		go h.StartMCPServer(protocol.Detach(ctx), cfg)
	}

	cfg.CreatedAt, cfg.UpdatedAt = now, now
	return map[string]any{
		"id":         extID,
		"type":       "mcp",
		"server":     cfg,
		"catalog_id": req.CatalogID,
	}, nil
}

func (h *PluginHandler) installMCPExtension(w http.ResponseWriter, r *http.Request,
	extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) {
	resp, err := h.internalInstallMCP(r.Context(), extID, entry, req, now)
	if err != nil {
		http.Error(w, "mcp_servers insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// installGenericExtension 安装 skill / plugin / app：写 extension_instances。
// skill/plugin 需异步下载文件并写运行时表（TODO: downloadAndInstall goroutine）。
func (h *PluginHandler) internalInstallGeneric(ctx context.Context, extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) (any, error) {
	name := cond(req.Name != "", req.Name, entry.Name)
	url := cond(req.URL != "", req.URL, entry.URL)

	configMap := map[string]any{
		"url":        url,
		"repo_url":   url,
		"entrypoint": "",
	}
	if entry.Version != "" {
		configMap["version"] = entry.Version
	}
	configJSON, _ := json.Marshal(configMap)

	status := "installed"
	if entry.Type == "skill" || entry.Type == "plugin" {
		status = "downloading"
	}

	installReq := marketplace.InstallRequest{
		Principal:   "system",
		ExtensionID: extID,
		CatalogID:   req.CatalogID,
		Name:        name,
		ExtType:     entry.Type,
		TrustTier:   entry.TrustTier,
		Publisher:   entry.Publisher,
		Config:      string(configJSON),
		RuntimeID:   "",
	}

	if err := h.InstallMgr.InstallExtension(ctx, installReq); err != nil {
		return nil, fmt.Errorf("Server.internalInstallGeneric: %w", err)
	}

	if entry.Type == "skill" || entry.Type == "plugin" {
		go h.downloadAndInstallExtension(protocol.Detach(ctx), extID, req.CatalogID, entry, now, name)
	}

	return map[string]any{
		"id":         extID,
		"type":       entry.Type,
		"name":       name,
		"publisher":  entry.Publisher,
		"trust_tier": entry.TrustTier,
		"catalog_id": req.CatalogID,
		"status":     status,
		"created_at": now,
	}, nil
}

func (h *PluginHandler) installGenericExtension(w http.ResponseWriter, r *http.Request,
	extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) {
	resp, err := h.internalInstallGeneric(r.Context(), extID, entry, req, now)
	if err != nil {
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
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
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	req.ID = "mp_" + hex.EncodeToString(b)
	req.IsBuiltin = 0
	req.TrustTier = 2 // Community
	req.Enabled = 1
	req.CreatedAt = now

	// 新增市场排在所有现有市场之后：取当前最大 sort_order + 10，留出调整空间
	maxOrder, _ := h.ExtRepo.GetMaxMarketplaceSortOrder(r.Context())
	req.SortOrder = maxOrder + 10

	err := h.ExtRepo.CreateMarketplace(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
//nolint:nestif
func (h *PluginHandler) downloadAndInstallExtension(ctx context.Context, extID, catalogID string, entry *protocol.RegistryEntry, now, name string) { //nolint:gocyclo,nestif
	// 1. 获取本地 tmp 目录路径
	// marketplace_id 本身可含 "/"（如 "polarisagi/polaris-plugins-official"），
	// 不能在第一个 "/" 处分割，必须从 extension_catalog 读取准确值。
	var mpID string
	if err := h.DB.QueryRowContext(ctx,
		`SELECT marketplace_id FROM extension_catalog WHERE id=?`, catalogID).Scan(&mpID); err != nil {
		h.updateExtensionInstanceError(ctx, extID, "catalog entry not found: "+err.Error())
		return
	}
	relPath := filepath.FromSlash(strings.TrimPrefix(catalogID, mpID+"/"))

	safeMpID := strings.ReplaceAll(mpID, "/", "_")
	srcDir := filepath.Join(h.DataDir, "tmp", "marketplaces", safeMpID, relPath)
	destDir := filepath.Join(h.DataDir, "extensions", extID)

	// 2. 拷贝目录
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		h.updateExtensionInstanceError(ctx, extID, err.Error())
		return
	}
	if err := copyDir(srcDir, destDir); err != nil {
		// 回退尝试 git sparse checkout 或者直接报错。当前假定 sync 已经拉取好了全量。
		h.updateExtensionInstanceError(ctx, extID, "failed to copy from tmp: "+err.Error())
		return
	}

	runtimeID := ""
	// 3. 路由到对应的运行时表
	if entry.Type == "skill" {
		// 命名规范：skill:{hex}，与 SkillRegistry 的 "skill:" 前缀约束对齐，同时保持全局唯一
		runtimeID = "skill:" + extID[4:]
		if h.SkillReg == nil {
			h.updateExtensionInstanceError(ctx, extID, "skill registry not available")
			return
		}
		skillMDBytes, err := os.ReadFile(filepath.Join(destDir, "SKILL.md"))
		if err != nil {
			h.updateExtensionInstanceError(ctx, extID, "SKILL.md not found")
			return
		}
		fm := parseFrontmatter(string(skillMDBytes))
		if fm.Description == "" {
			fm.Description = entry.Description
		}
		caps := []string{"description:" + fm.Description}
		if fm.Capability != "" {
			caps = append(caps, "capability:"+fm.Capability)
		}
		meta := apptypes.SkillMeta{
			Name:         runtimeID,
			Version:      fm.Version,
			Runtime:      "script",
			RiskLevel:    fm.RiskLevel,
			Sandbox:      sandboxLevel(fm.Sandbox),
			Capabilities: caps,
			ExecMode:     fm.ExecMode,
			Trust:        apptypes.TrustTier(entry.TrustTier),
			Instructions: string(skillMDBytes),
			PluginID:     "", // 独立安装，无插件归属
		}
		if err := h.SkillReg.Register(ctx, meta); err != nil {
			h.updateExtensionInstanceError(ctx, extID, "register skill: "+err.Error())
			return
		}
	} else if entry.Type == "plugin" {
		runtimeID = "pl_" + extID[4:]

		// 解析 Bundle 清单（Polaris 原生格式优先；权威来源是文件系统，DB 只做快照缓存）
		var bundle protocol.PluginBundleManifest
		var manifestRaw []byte
		for _, manifestPath := range []string{
			filepath.Join(destDir, ".polaris-plugin", "plugin.json"),
			filepath.Join(destDir, "plugin.json"),
		} {
			if raw, err2 := os.ReadFile(manifestPath); err2 == nil {
				manifestRaw = raw
				_ = json.Unmarshal(raw, &bundle)
				break
			}
		}
		if manifestRaw == nil {
			manifestRaw = []byte("{}")
		}

		// 收集插件内所有 MCP 服务器定义（三种来源），写入 mcp_servers 表并构建 mcp_policy。
		// mcp_policy 仅存储额外策略（approval_mode、enabled_tools 等），enabled 状态由 mcp_servers.enabled 权威管理。
		mcpPolicyMap := make(map[string]map[string]any)
		allMCPs := make(map[string]pluginMCPDef)

		for srvName, def := range bundle.MCPInline {
			allMCPs[srvName] = pluginMCPDef{Command: def.Command, Args: def.Args, Env: def.Env, URL: def.URL}
			mcpPolicyMap[srvName] = map[string]any{}
		}
		if bundle.MCPFile != "" {
			if safePath, ok := safeJoin(destDir, bundle.MCPFile); ok {
				if mcpCfg, err2 := marketplace.LoadMCPConfig(safePath); err2 == nil {
					for srvName, def := range mcpCfg.MCPServers {
						if _, exists := allMCPs[srvName]; !exists {
							allMCPs[srvName] = pluginMCPDef{Command: def.Command, Args: def.Args, Env: def.Env, URL: def.URL}
							mcpPolicyMap[srvName] = map[string]any{}
						}
					}
				}
			}
		}
		// 兼容第三方格式（OpenAI ai-plugin.json / Anthropic plugin.toml 等）
		if subEntries, err2 := marketplace.ParseManifestDir(destDir, "", protocol.Marketplace{
			ID: "bundle_" + extID, Publisher: entry.Publisher, TrustTier: entry.TrustTier,
		}); err2 == nil {
			for _, sub := range subEntries {
				if sub.Type == "mcp" && sub.Command != "" {
					if _, exists := allMCPs[sub.Name]; !exists {
						allMCPs[sub.Name] = pluginMCPDef{Command: sub.Command, Args: sub.Args, URL: sub.URL}
						mcpPolicyMap[sub.Name] = map[string]any{}
					}
				}
			}
		}
		mcpPolicyBytes, _ := json.Marshal(mcpPolicyMap)

		// 从 bundle interface 字段提取展示信息
		displayName := name
		homepage := ""
		if bundle.Interface != nil {
			if bundle.Interface.DisplayName != "" {
				displayName = bundle.Interface.DisplayName
			}
			homepage = bundle.Interface.WebsiteURL
		}

		err := h.ExtRepo.UpsertPlugin(ctx, runtimeID, name, bundle.Version, displayName, entry.Description,
			entry.Publisher, homepage, destDir, 1, entry.TrustTier, catalogID, string(mcpPolicyBytes), string(manifestRaw), now, now)
		if err != nil {
			h.updateExtensionInstanceError(ctx, extID, "insert plugin err: "+err.Error())
			return
		}

		// 注册插件内置的 skills（agentskills.io 标准：skills 是插件 bundle 的一等组件）
		h.registerPluginSkills(ctx, runtimeID, name, destDir, &bundle, entry.TrustTier)

		// 将插件内嵌的 MCP 写入 mcp_servers 表并异步启动（统一架构，State-in-DB）
		h.registerPluginMCPServers(ctx, runtimeID, name, destDir, allMCPs, entry.TrustTier, now)

		if hook, ok := bundle.Hooks["install"]; ok && hook != "" {
			if hookPath, ok := safeJoin(destDir, hook); ok {
				if h.ScriptRunner != nil {
					// ContainerSandbox.RunScript：Linux 下有 PID/NS namespace 隔离
					if err := h.ScriptRunner.RunScript(ctx, hookPath, destDir); err != nil {
						slog.Warn("plugin_catalog: install hook failed", "ext", extID, "err", err)
					}
				} else {
					// scriptRunner 未注入（如 Tier-0 macOS 无 L3）：skip，记录警告
					slog.Warn("plugin_catalog: install hook skipped (no scriptRunner, call SetScriptRunner to enable)",
						"ext", extID, "hook", hookPath)
				}
			}
		}
	}

	// 4. 更新 extension_instances 为 installed
	_ = h.InstallMgr.UpdateInstance(ctx, extID, marketplace.InstanceUpdate{
		Status:      "installed",
		RuntimeID:   runtimeID,
		InstallPath: destDir,
		ClearError:  true,
	})
}

func (h *PluginHandler) updateExtensionInstanceError(ctx context.Context, extID, errMsg string) {
	if h.InstallMgr != nil {
		_ = h.InstallMgr.UpdateInstance(ctx, extID, marketplace.InstanceUpdate{
			Status:   "error",
			ErrorMsg: errMsg,
		})
	}
}

func copyDir(src string, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("copyDir: %w", err)
	}
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return fmt.Errorf("copyDir: %w", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("copyDir: %w", err)
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return fmt.Errorf("copyDir: %w", err)
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return fmt.Errorf("copyDir: %w", err)
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copyFile: %w", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("copyFile: %w", err)
	}
	return os.WriteFile(dst, data, info.Mode())
}

// pluginMCPDef 是 registerPluginMCPServers 的内部传参结构，避免与 protocol 包产生循环依赖。
type pluginMCPDef struct {
	Command string
	Args    []string
	Env     map[string]string
	URL     string
}

// registerPluginMCPServers 将插件 bundle 中所有 MCP 服务器写入 mcp_servers 全局表，
// 并异步启动连接（agentskills.io 标准：plugin 安装时 MCP 自动注册到统一表）。
func (h *PluginHandler) registerPluginMCPServers(ctx context.Context, pluginID, pluginName, installPath string, servers map[string]pluginMCPDef, trustTier int, now string) {
	for srvName, def := range servers {
		transport := "stdio"
		if def.URL != "" {
			transport = "streamable_http"
		}
		argsBytes, _ := json.Marshal(def.Args)
		envMap := def.Env
		if envMap == nil {
			envMap = map[string]string{}
		}
		envBytes, _ := json.Marshal(envMap)
		serverID := fmt.Sprintf("plugin_%s_%s", pluginID, srvName)
		// scopedName：srvName 已是 pluginName 后缀时直接用 pluginName（避免双后缀如
		// "polaris-social-poster-social-poster"）；否则拼接区分同插件多服务器场景。
		scopedName := pluginName
		if pluginName != srvName && !strings.HasSuffix(pluginName, "-"+srvName) {
			scopedName = pluginName + "-" + srvName
		}

		err := h.ExtRepo.UpsertMCPServer(ctx, apptypes.MCPServerRow{
			ID:        serverID,
			Name:      scopedName,
			Transport: transport,
			Command:   def.Command,
			Args:      string(argsBytes),
			Env:       string(envBytes),
			URL:       def.URL,
			Enabled:   true,
			Timeout:   30,
			TrustTier: trustTier,
			CatalogID: "",
			PluginID:  pluginID,
			WorkDir:   installPath,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			slog.Warn("plugin_catalog: register plugin mcp failed", "server", srvName, "err", err)
			continue
		}

		if h.MCPMgr != nil {
			cfg := types.MCPServerConfig{
				ID: serverID, Name: scopedName, Transport: transport,
				Command: def.Command, Args: def.Args, Env: def.Env,
				URL: def.URL, Timeout: 30, WorkDir: installPath,
			}
			//nolint:errcheck
			go h.StartMCPServer(protocol.Detach(ctx), cfg)
		}
	}
}

// registerPluginSkills 扫描插件 bundle 中声明的 skills 目录，
// 将每个 SKILL.md 注册进 skills 表（agentskills.io 标准：plugin 安装时 skills 自动发现）。
// skillReg 为 nil 时静默跳过（Tier-0 降级）。
func (h *PluginHandler) registerPluginSkills(ctx context.Context, pluginID, pluginName, destDir string, bundle *protocol.PluginBundleManifest, trustTier int) {
	if h.SkillReg == nil {
		return
	}

	// 确定 skills 根目录：manifest 声明 > 约定路径 "skills/"
	skillsRoot := ""
	if bundle.SkillsDir != "" {
		if p, ok := safeJoin(destDir, bundle.SkillsDir); ok {
			skillsRoot = p
		}
	}
	if skillsRoot == "" {
		candidate := filepath.Join(destDir, "skills")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			skillsRoot = candidate
		}
	}

	// array form（bundle.Skills）：每个 BundleSkillRef.Path 直接指向 SKILL.md
	if len(bundle.Skills) > 0 {
		for _, ref := range bundle.Skills {
			if ref.Path == "" {
				continue
			}
			skillMDPath, ok := safeJoin(destDir, ref.Path)
			if !ok {
				continue
			}
			h.registerOneSkill(ctx, pluginID, pluginName, skillMDPath, trustTier)
		}
		return
	}

	// string form：遍历 skillsRoot 下所有 SKILL.md
	if skillsRoot == "" {
		return
	}
	_ = filepath.WalkDir(skillsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
			return nil //nolint:nilerr
		}
		h.registerOneSkill(ctx, pluginID, pluginName, path, trustTier)
		return nil
	})
}

// registerOneSkill 读取单个 SKILL.md 并写入 skills 表。
func (h *PluginHandler) registerOneSkill(ctx context.Context, pluginID, pluginName, skillMDPath string, trustTier int) {
	data, err := os.ReadFile(skillMDPath)
	if err != nil {
		slog.Warn("plugin_catalog: cannot read SKILL.md", "path", skillMDPath, "err", err)
		return
	}
	fm := parseFrontmatter(string(data))

	// skill 名称：优先 frontmatter name；否则取目录名
	skillSlug := fm.Name
	if skillSlug == "" {
		skillSlug = filepath.Base(filepath.Dir(skillMDPath))
	}
	// 命名规范：skill:{plugin-name}/{skill-slug}（与 agentskills.io 命名空间一致）
	fullName := "skill:" + pluginName + "/" + skillSlug

	version := fm.Version
	caps := []string{}
	if fm.Capability != "" {
		caps = append(caps, "capability:"+fm.Capability)
	}

	meta := apptypes.SkillMeta{
		Name:         fullName,
		Version:      version,
		Runtime:      "script",
		RiskLevel:    fm.RiskLevel,
		Sandbox:      sandboxLevel(fm.Sandbox),
		Capabilities: caps,
		ExecMode:     fm.ExecMode,
		Trust:        apptypes.TrustTier(trustTier),
		Instructions: string(data),
		PluginID:     pluginID,
	}

	if err := h.SkillReg.Register(ctx, meta); err != nil {
		slog.Warn("plugin_catalog: register skill failed", "skill", fullName, "err", err)
	}
}

// safeJoin 将 rel 拼接到 base 下，并通过 EvalSymlinks + Rel 验证结果仍在 base 内。
// 防止 "../" 路径穿越；返回 (resolvedPath, true) 或 ("", false)。
func safeJoin(base, rel string) (string, bool) {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", false
	}
	// filepath.Clean("/" + rel) 将 rel 规范化为绝对形式再去掉前导 "/"，
	// 从而让 "../../etc/passwd" 变成 "/etc/passwd"，filepath.Join 后安全可比较。
	candidate := filepath.Join(resolvedBase, filepath.Clean("/"+rel))
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// 文件尚不存在时 EvalSymlinks 会报错；仅做静态检查
		realCandidate = candidate
	}
	relPart, err := filepath.Rel(resolvedBase, realCandidate)
	if err != nil || strings.HasPrefix(relPart, "..") || filepath.IsAbs(relPart) {
		return "", false
	}
	return realCandidate, true
}
