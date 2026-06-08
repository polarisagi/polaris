package server

import (
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

	"github.com/polarisagi/polaris/pkg/extensions/marketplace"

	"github.com/polarisagi/polaris/internal/protocol"
)

// getInstalledCatalogIDs 返回所有已安装的 catalog_id 到 installed_version 的映射。
// SSoT：仅查 extension_instances，不再 UNION 多表。
func (s *Server) getInstalledCatalogIDs(ctx context.Context) map[string]string {
	installed := map[string]string{}
	rows, err := s.db.QueryContext(ctx,
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

// appendCustomCatalogs 追加用户自建扩展（origin=user）到目录列表。
// 全走 extension_instances，不再散查 skills/plugins/apps 三表。
func (s *Server) appendCustomCatalogs(ctx context.Context, result []protocol.RegistryEntry, _ map[string]string) []protocol.RegistryEntry {
	rows, err := s.db.QueryContext(ctx,
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

// appendCachedCatalogs 追加市场同步缓存条目，叠加安装状态。
func (s *Server) appendCachedCatalogs(ctx context.Context, result []protocol.RegistryEntry, installed map[string]string) []protocol.RegistryEntry {
	rows, err := s.db.QueryContext(ctx, `
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

// handleListPluginCatalog 返回扩展目录列表（用户自建 + 市场缓存）。
// 排序规则：已安装优先 → 官方市场优先（SortOrder == 0）→ 名字字母序。
// 已安装的条目只出现一次（installed=true），不在未安装区重复展示。
// GET /v1/plugins/catalog
func (s *Server) handleListPluginCatalog(w http.ResponseWriter, r *http.Request) {
	installed := s.getInstalledCatalogIDs(r.Context())
	result := make([]protocol.RegistryEntry, 0)
	result = s.appendCustomCatalogs(r.Context(), result, installed)
	result = s.appendCachedCatalogs(r.Context(), result, installed)

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

// handleInstallPlugin 一键安装目录条目。
// MCP → mcp_servers + extension_instances；Skill/Plugin → extension_instances（异步下载）。
// POST /v1/plugins/install
func (s *Server) handleInstallPlugin(w http.ResponseWriter, r *http.Request) { //nolint:nestif
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
	err := s.db.QueryRowContext(r.Context(),
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
	s.db.QueryRowContext(r.Context(), //nolint:errcheck
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
	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx := FromContext(r.Context())
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
	if err := s.installMgr.InstallExtension(r.Context(), installReq); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			// Trigger HITL via hitlGateway
			if s.hitlGateway != nil {
				_, _ = s.hitlGateway.Prompt(r.Context(), protocol.HITLPrompt{
					ID:             extID,
					CheckpointType: "security_review",
					PromptText:     "Approve installation for extension: " + entry.Name,
					Options: []protocol.HITLOption{
						{Key: "approve", Label: "Approve"},
						{Key: "deny", Label: "Deny"},
					},
				})
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
		s.installMCPExtension(w, r, extID, &entry, req, now)
	default: // skill | plugin | app
		s.installGenericExtension(w, r, extID, &entry, req, now)
	}
	s.clearToolSchemaCache()
	slog.Info("marketplace: installed extension", "id", extID, "type", entry.Type, "name", entry.Name)
}

// installMCPExtension 安装 MCP 类型：写 extension_instances + mcp_servers + 异步启动。
func (s *Server) internalInstallMCP(ctx context.Context, extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) (any, error) {
	cfg := MCPServerConfig{
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

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO extension_instances
		 (id, ext_type, origin, catalog_id, name, publisher, trust_tier,
		  runtime_id, install_path, config, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,'installed',?,?)`,
		extID, "mcp", "marketplace", req.CatalogID,
		cfg.Name, entry.Publisher, entry.TrustTier,
		mcpID, "", string(configJSON), now, now)
	if err != nil {
		return nil, err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mcp_servers(id, name, transport, command, args, env, url, enabled, timeout, trust_tier, catalog_id, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,1,?,?,?,?,?)`,
		mcpID, cfg.Name, cfg.Transport, cfg.Command,
		string(argsBytes), string(envBytes),
		cfg.URL, cfg.Timeout, cfg.TrustTier, req.CatalogID, now, now)
	if err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM extension_instances WHERE id=?`, extID)
		return nil, err
	}

	if s.mcpMgr != nil {
		go s.startMCPServer(cfg)
	}

	cfg.CreatedAt, cfg.UpdatedAt = now, now
	return map[string]any{
		"id":         extID,
		"type":       "mcp",
		"server":     cfg,
		"catalog_id": req.CatalogID,
	}, nil
}

func (s *Server) installMCPExtension(w http.ResponseWriter, r *http.Request,
	extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) {
	resp, err := s.internalInstallMCP(r.Context(), extID, entry, req, now)
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
func (s *Server) internalInstallGeneric(ctx context.Context, extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) (any, error) {
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

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO extension_instances
		 (id, ext_type, origin, catalog_id, name, publisher, trust_tier,
		  runtime_id, install_path, config, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,'','',?,?,?,?)`,
		extID, entry.Type, "marketplace", req.CatalogID,
		name, entry.Publisher, entry.TrustTier,
		string(configJSON), status, now, now)
	if err != nil {
		return nil, err
	}

	if entry.Type == "skill" || entry.Type == "plugin" {
		go s.downloadAndInstallExtension(context.Background(), extID, req.CatalogID, entry, now, name)
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

func (s *Server) installGenericExtension(w http.ResponseWriter, r *http.Request,
	extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) {
	resp, err := s.internalInstallGeneric(r.Context(), extID, entry, req, now)
	if err != nil {
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleUninstallPlugin 卸载扩展（通过 catalog_id 定位 extension_instances）。
// DELETE /v1/plugins/{catalogID}
func (s *Server) handleUninstallPlugin(w http.ResponseWriter, r *http.Request) {
	catalogID := r.PathValue("catalogID")

	if s.installMgr == nil {
		http.Error(w, "marketplace manager not initialized", http.StatusInternalServerError)
		return
	}

	err := s.installMgr.UninstallExtension(r.Context(), catalogID)
	if err != nil {
		if strings.Contains(err.Error(), "not installed") {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	s.clearToolSchemaCache()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "uninstalled"}) //nolint:errcheck
}

// Marketplace CRUD ---------------------------------------------------------

// handleListMarketplaces GET /v1/plugins/marketplaces
func (s *Server) handleListMarketplaces(w http.ResponseWriter, r *http.Request) {
	var mps []protocol.Marketplace
	rows, err := s.db.QueryContext(r.Context(),
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

// handleAddMarketplace POST /v1/plugins/marketplaces
func (s *Server) handleAddMarketplace(w http.ResponseWriter, r *http.Request) {
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
	var maxOrder int
	_ = s.db.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(sort_order), 90) FROM plugin_marketplaces`).Scan(&maxOrder)
	req.SortOrder = maxOrder + 10

	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO plugin_marketplaces
		 (id, name, type, publisher, repo_url, description, is_builtin, trust_tier, enabled, sort_order, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		req.ID, req.Name, req.Type, req.Publisher, req.RepoURL,
		req.Description, req.IsBuiltin, req.TrustTier, req.Enabled, req.SortOrder, req.CreatedAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(req)
}

// handleDeleteMarketplace DELETE /v1/plugins/marketplaces/{id}
func (s *Server) handleDeleteMarketplace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.db.ExecContext(r.Context(),
		`DELETE FROM plugin_marketplaces WHERE id=? AND is_builtin=0`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
func (s *Server) downloadAndInstallExtension(ctx context.Context, extID, catalogID string, entry *protocol.RegistryEntry, now, name string) { //nolint:gocyclo,nestif
	// 1. 获取本地 tmp 目录路径
	// marketplace_id 本身可含 "/"（如 "polarisagi/polaris-plugins-official"），
	// 不能在第一个 "/" 处分割，必须从 extension_catalog 读取准确值。
	var mpID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT marketplace_id FROM extension_catalog WHERE id=?`, catalogID).Scan(&mpID); err != nil {
		s.updateExtensionInstanceError(ctx, extID, "catalog entry not found: "+err.Error())
		return
	}
	relPath := filepath.FromSlash(strings.TrimPrefix(catalogID, mpID+"/"))

	safeMpID := strings.ReplaceAll(mpID, "/", "_")
	srcDir := filepath.Join(s.dataDir, "tmp", "marketplaces", safeMpID, relPath)
	destDir := filepath.Join(s.dataDir, "extensions", extID)

	// 2. 拷贝目录
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		s.updateExtensionInstanceError(ctx, extID, err.Error())
		return
	}
	if err := copyDir(srcDir, destDir); err != nil {
		// 回退尝试 git sparse checkout 或者直接报错。当前假定 sync 已经拉取好了全量。
		s.updateExtensionInstanceError(ctx, extID, "failed to copy from tmp: "+err.Error())
		return
	}

	runtimeID := ""
	// 3. 路由到对应的运行时表
	if entry.Type == "skill" {
		// 命名规范：skill:{hex}，与 SkillRegistry 的 "skill:" 前缀约束对齐，同时保持全局唯一
		runtimeID = "skill:" + extID[4:]
		if s.skillReg == nil {
			s.updateExtensionInstanceError(ctx, extID, "skill registry not available")
			return
		}
		skillMDBytes, err := os.ReadFile(filepath.Join(destDir, "SKILL.md"))
		if err != nil {
			s.updateExtensionInstanceError(ctx, extID, "SKILL.md not found")
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
		meta := protocol.SkillMeta{
			Name:         runtimeID,
			Version:      fm.Version,
			Runtime:      "script",
			RiskLevel:    fm.RiskLevel,
			Sandbox:      sandboxLevel(fm.Sandbox),
			Capabilities: caps,
			ExecMode:     fm.ExecMode,
			Trust:        protocol.TrustTier(entry.TrustTier),
			Instructions: string(skillMDBytes),
			PluginID:     "", // 独立安装，无插件归属
		}
		if err := s.skillReg.Register(ctx, meta); err != nil {
			s.updateExtensionInstanceError(ctx, extID, "register skill: "+err.Error())
			return
		}
	} else if entry.Type == "plugin" {
		runtimeID = "pl_" + extID[4:]

		// 解析 Bundle 清单（Polaris 原生格式优先；权威来源是文件系统，DB 只做快照缓存）
		var bundle protocol.PluginBundleManifest
		var manifestRaw []byte
		for _, manifestPath := range []string{
			filepath.Join(destDir, ".codex-plugin", "plugin.json"),
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

		_, err := s.db.ExecContext(ctx,
			`INSERT INTO plugins(id, name, version, display_name, description, publisher, homepage,
			  install_path, enabled, trust_tier, catalog_id, mcp_policy, manifest, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,1,?,?,?,?,?,?)`,
			runtimeID, name, bundle.Version, displayName, entry.Description,
			entry.Publisher, homepage, destDir,
			entry.TrustTier, catalogID, string(mcpPolicyBytes), string(manifestRaw), now, now)
		if err != nil {
			s.updateExtensionInstanceError(ctx, extID, "insert plugin err: "+err.Error())
			return
		}

		// 注册插件内置的 skills（agentskills.io 标准：skills 是插件 bundle 的一等组件）
		s.registerPluginSkills(ctx, runtimeID, name, destDir, &bundle, entry.TrustTier)

		// 将插件内嵌的 MCP 写入 mcp_servers 表并异步启动（统一架构，State-in-DB）
		s.registerPluginMCPServers(ctx, runtimeID, name, destDir, allMCPs, entry.TrustTier, now)

		if hook, ok := bundle.Hooks["install"]; ok && hook != "" {
			if hookPath, ok := safeJoin(destDir, hook); ok {
				if s.scriptRunner != nil {
					// ContainerSandbox.RunScript：Linux 下有 PID/NS namespace 隔离
					if err := s.scriptRunner.RunScript(ctx, hookPath, destDir); err != nil {
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
	_, _ = s.db.ExecContext(ctx,
		`UPDATE extension_instances SET status='installed', runtime_id=?, install_path=?, updated_at=? WHERE id=?`,
		runtimeID, destDir, time.Now().UTC().Format(time.RFC3339), extID)
}

func (s *Server) updateExtensionInstanceError(ctx context.Context, extID, errMsg string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE extension_instances SET status='error', error_msg=?, updated_at=? WHERE id=?`,
		errMsg, time.Now().UTC().Format(time.RFC3339), extID)
}

func copyDir(src string, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
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
func (s *Server) registerPluginMCPServers(ctx context.Context, pluginID, pluginName, installPath string, servers map[string]pluginMCPDef, trustTier int, now string) {
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
		// 命名与 LoadOnePlugin 时的 scopedName 逻辑保持一致
		scopedName := pluginName
		if pluginName != srvName {
			scopedName = pluginName + "-" + srvName
		}

		_, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO mcp_servers
			 (id, name, transport, command, args, env, url, enabled, timeout,
			  trust_tier, catalog_id, plugin_id, work_dir, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,1,30,?,?,?,?,?,?)`,
			serverID, scopedName, transport, def.Command,
			string(argsBytes), string(envBytes), def.URL,
			trustTier, "", pluginID, installPath, now, now)
		if err != nil {
			slog.Warn("plugin_catalog: register plugin mcp failed", "server", srvName, "err", err)
			continue
		}

		if s.mcpMgr != nil {
			cfg := MCPServerConfig{
				ID: serverID, Name: scopedName, Transport: transport,
				Command: def.Command, Args: def.Args, Env: def.Env,
				URL: def.URL, Timeout: 30, WorkDir: installPath,
			}
			go s.startMCPServer(cfg)
		}
	}
}

// registerPluginSkills 扫描插件 bundle 中声明的 skills 目录，
// 将每个 SKILL.md 注册进 skills 表（agentskills.io 标准：plugin 安装时 skills 自动发现）。
// skillReg 为 nil 时静默跳过（Tier-0 降级）。
func (s *Server) registerPluginSkills(ctx context.Context, pluginID, pluginName, destDir string, bundle *protocol.PluginBundleManifest, trustTier int) {
	if s.skillReg == nil {
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
			s.registerOneSkill(ctx, pluginID, pluginName, skillMDPath, trustTier)
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
		s.registerOneSkill(ctx, pluginID, pluginName, path, trustTier)
		return nil
	})
}

// registerOneSkill 读取单个 SKILL.md 并写入 skills 表。
func (s *Server) registerOneSkill(ctx context.Context, pluginID, pluginName, skillMDPath string, trustTier int) {
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

	meta := protocol.SkillMeta{
		Name:         fullName,
		Version:      version,
		Runtime:      "script",
		RiskLevel:    fm.RiskLevel,
		Sandbox:      sandboxLevel(fm.Sandbox),
		Capabilities: caps,
		ExecMode:     fm.ExecMode,
		Trust:        protocol.TrustTier(trustTier),
		Instructions: string(data),
		PluginID:     pluginID,
	}

	if err := s.skillReg.Register(ctx, meta); err != nil {
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
