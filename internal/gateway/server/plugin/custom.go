package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"

	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	apptypes "github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// HandleCreateSkill 用户手动创建 Skill 扩展。
// POST /v1/skills/create
func (h *PluginHandler) HandleCreateSkill(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		RepoURL     string `json:"repo_url"`
		Entrypoint  string `json:"entrypoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	extID := util.GenerateHumanReadableID("ext", req.Name)

	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx0 := authcontext.FromContext(r.Context())
	principal0 := authCtx0.UserID
	if principal0 == "" {
		principal0 = "user"
	}
	installReq0 := marketplace.InstallRequest{
		Principal:   principal0,
		ExtensionID: extID,
		ExtType:     "skill",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := h.InstallMgr.Authorize(r.Context(), installReq0); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if h.HITLGateway != nil {
				bgCtx, cancel := context.WithTimeout(protocol.Detach(r.Context()), 30*time.Minute)
				go func() {
					defer cancel()
					resp, err := h.HITLGateway.Prompt(bgCtx, apptypes.HITLPrompt{
						ID:             extID,
						CheckpointType: "security_review",
						PromptText:     "Approve creation for custom skill: " + req.Name,
						Options: []apptypes.HITLOption{
							{Key: "approve", Label: "Approve"},
							{Key: "deny", Label: "Deny"},
						},
					})
					if err == nil && resp != nil && resp.Approved {
						configJSON, _ := json.Marshal(map[string]any{
							"repo_url":   req.RepoURL,
							"entrypoint": req.Entrypoint,
						})
						installReq0.Name = req.Name
						installReq0.Config = string(configJSON)
						installReq0.BypassAuth = true
						_ = h.InstallMgr.InstallExtension(bgCtx, installReq0)
						slog.Info("plugin_custom: custom skill installed via HITL", "id", extID)
					}
				}()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{
		"repo_url":   req.RepoURL,
		"entrypoint": req.Entrypoint,
	})

	installReq0.Name = req.Name
	installReq0.Config = string(configJSON)
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq0); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "name": req.Name, "type": "skill",
	})
}

// HandleCreatePlugin 用户手动创建 Plugin 扩展，支持两种模式：
//   - manifest_url 模式：传入 manifest_url，直接安装已有插件。
//   - intent 模式：传入 intent，由 PluginCreator（M2）调用 LLM 生成 MCP 插件代码，
//     并自动注册为本地 MCP Server（写 mcp_servers + extension_instances 表）。
//
// POST /v1/plugins/create
func (h *PluginHandler) HandleCreatePlugin(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ManifestURL string `json:"manifest_url"`
		Intent      string `json:"intent"` // LLM 驱动生成：描述插件意图，留空则走 manifest_url 模式
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	extID := util.GenerateHumanReadableID("ext", req.Name)

	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx1 := authcontext.FromContext(r.Context())
	principal1 := authCtx1.UserID
	if principal1 == "" {
		principal1 = "user"
	}
	installReq1 := marketplace.InstallRequest{
		Principal:   principal1,
		ExtensionID: extID,
		ExtType:     "plugin",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := h.InstallMgr.Authorize(r.Context(), installReq1); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if h.HITLGateway != nil {
				bgCtx, cancel := context.WithTimeout(protocol.Detach(r.Context()), 30*time.Minute)
				go func() {
					defer cancel()
					resp, err := h.HITLGateway.Prompt(bgCtx, apptypes.HITLPrompt{
						ID:             extID,
						CheckpointType: "security_review",
						PromptText:     "Approve creation for custom plugin: " + req.Name,
						Options: []apptypes.HITLOption{
							{Key: "approve", Label: "Approve"},
							{Key: "deny", Label: "Deny"},
						},
					})
					if err == nil && resp != nil && resp.Approved {
						configJSON, _ := json.Marshal(map[string]any{
							"manifest_url": req.ManifestURL,
							"intent":       req.Intent,
							"description":  req.Description,
						})
						installReq1.Name = req.Name
						installReq1.Config = string(configJSON)
						installReq1.BypassAuth = true
						_ = h.InstallMgr.InstallExtension(bgCtx, installReq1)
						slog.Info("plugin_custom: custom plugin installed via HITL", "id", extID)
					}
				}()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// ── intent 模式：LLM 生成 MCP 插件 → 注册为本地 MCP Server ───────────────
	if req.Intent != "" && h.PluginCreator != nil {
		h.HandleCreatePluginFromIntent(w, r, extID, installReq1, req.Intent)
		return
	}

	// ── manifest_url 模式：直接安装已有插件 ───────────────────────────────────
	configJSON, _ := json.Marshal(map[string]any{
		"manifest_url": req.ManifestURL,
		"intent":       req.Intent,
		"description":  req.Description,
	})

	installReq1.Name = req.Name
	installReq1.Config = string(configJSON)
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq1); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "name": req.Name, "type": "plugin",
	})
}

// HandleCreateApp 用户手动创建 App 扩展（URL 模式）。
// POST /v1/apps/create
func (h *PluginHandler) HandleCreateApp(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	extID := util.GenerateHumanReadableID("ext", req.Name)

	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx2 := authcontext.FromContext(r.Context())
	principal2 := authCtx2.UserID
	if principal2 == "" {
		principal2 = "user"
	}
	installReq2 := marketplace.InstallRequest{
		Principal:   principal2,
		ExtensionID: extID,
		ExtType:     "app",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := h.InstallMgr.Authorize(r.Context(), installReq2); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if h.HITLGateway != nil {
				bgCtx, cancel := context.WithTimeout(protocol.Detach(r.Context()), 30*time.Minute)
				go func() {
					defer cancel()
					resp, err := h.HITLGateway.Prompt(bgCtx, apptypes.HITLPrompt{
						ID:             extID,
						CheckpointType: "security_review",
						PromptText:     "Approve creation for custom app: " + req.Name,
						Options: []apptypes.HITLOption{
							{Key: "approve", Label: "Approve"},
							{Key: "deny", Label: "Deny"},
						},
					})
					if err == nil && resp != nil && resp.Approved {
						appID := util.GenerateHumanReadableID("app", req.Name)
						err := h.ExtRepo.UpsertApp(bgCtx, apptypes.AppRow{
							ID:          appID,
							Name:        req.Name,
							DisplayName: req.Name,
							Description: req.Description,
							URL:         req.URL,
							Publisher:   "user",
							Enabled:     true,
							TrustTier:   1,
							CatalogID:   "",
							CreatedAt:   now,
							UpdatedAt:   now,
						})
						if err == nil {
							configJSON, _ := json.Marshal(map[string]any{"url": req.URL})
							installReq2.Name = req.Name
							installReq2.Config = string(configJSON)
							installReq2.RuntimeID = appID
							installReq2.BypassAuth = true
							_ = h.InstallMgr.InstallExtension(bgCtx, installReq2)
							slog.Info("plugin_custom: custom app installed via HITL", "id", extID)
						}
					}
				}()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// 写 apps 运行时表（与 MCP→mcp_servers / Skill→skills 的模式一致）
	appID := util.GenerateHumanReadableID("app", req.Name)
	err := h.ExtRepo.UpsertApp(r.Context(), apptypes.AppRow{
		ID:          appID,
		Name:        req.Name,
		DisplayName: req.Name,
		Description: req.Description,
		URL:         req.URL,
		Publisher:   "user",
		Enabled:     true,
		TrustTier:   1,
		CatalogID:   "",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		http.Error(w, "apps insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{"url": req.URL})
	installReq2.Name = req.Name
	installReq2.Config = string(configJSON)
	installReq2.RuntimeID = appID
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq2); err != nil {
		// 回滚 apps 插入
		_ = h.ExtRepo.DeleteApp(r.Context(), appID)
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "app_id": appID, "name": req.Name, "type": "app",
	})
}

// HandleCreateMCP 用户手动配置 MCP Server。
// POST /v1/mcp/create
// MCP 需要实时连接，同时写 mcp_servers（运行时）和 extension_instances（安装 SSoT）。
func (h *PluginHandler) HandleCreateMCP(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
		URL       string            `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	mcpID := util.GenerateHumanReadableID("mcp", req.Name)
	extID := util.GenerateHumanReadableID("ext", req.Name)

	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx3 := authcontext.FromContext(r.Context())
	principal3 := authCtx3.UserID
	if principal3 == "" {
		principal3 = "user"
	}
	installReq3 := marketplace.InstallRequest{
		Principal:   principal3,
		ExtensionID: extID,
		ExtType:     "mcp",
		TrustTier:   1, // TrustLocal
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := h.InstallMgr.Authorize(r.Context(), installReq3); err != nil { //nolint:nestif
		if errors.Is(err, marketplace.ErrRequiresApproval) {
			if h.HITLGateway != nil {
				bgCtx, cancel := context.WithTimeout(protocol.Detach(r.Context()), 30*time.Minute)
				go func() {
					defer cancel()
					resp, err := h.HITLGateway.Prompt(bgCtx, apptypes.HITLPrompt{
						ID:             extID,
						CheckpointType: "security_review",
						PromptText:     "Approve creation for custom mcp: " + req.Name,
						Options: []apptypes.HITLOption{
							{Key: "approve", Label: "Approve"},
							{Key: "deny", Label: "Deny"},
						},
					})
					if err == nil && resp != nil && resp.Approved {
						argsBytes, _ := json.Marshal(req.Args)
						envBytes, _ := json.Marshal(req.Env)
						now := time.Now().UTC().Format(time.RFC3339)

						err := h.ExtRepo.UpsertMCPServer(bgCtx, apptypes.MCPServerRow{
							ID:        mcpID,
							Name:      req.Name,
							Transport: req.Transport,
							Command:   req.Command,
							Args:      string(argsBytes),
							Env:       string(envBytes),
							URL:       req.URL,
							Enabled:   true,
							Timeout:   30,
							TrustTier: 1,
							CatalogID: "",
							CreatedAt: now,
							UpdatedAt: now,
						})
						if err == nil {
							configJSON, _ := json.Marshal(map[string]any{"url": req.URL})
							installReq3.Name = req.Name
							installReq3.Config = string(configJSON)
							installReq3.RuntimeID = mcpID
							installReq3.BypassAuth = true
							_ = h.InstallMgr.InstallExtension(bgCtx, installReq3)
							slog.Info("plugin_custom: custom mcp installed via HITL", "id", extID)
						}
					}
				}()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	argsBytes, _ := json.Marshal(req.Args)
	envBytes, _ := json.Marshal(req.Env)

	err := h.ExtRepo.UpsertMCPServer(r.Context(), apptypes.MCPServerRow{
		ID:        mcpID,
		Name:      req.Name,
		Transport: req.Transport,
		Command:   req.Command,
		Args:      string(argsBytes),
		Env:       string(envBytes),
		URL:       req.URL,
		Enabled:   true,
		Timeout:   30,
		TrustTier: 1,
		CatalogID: "",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		http.Error(w, "mcp_servers insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	configJSON, _ := json.Marshal(map[string]any{
		"transport": req.Transport,
		"command":   req.Command,
		"args":      req.Args,
		"env":       req.Env,
		"url":       req.URL,
	})

	installReq3.Name = req.Name
	installReq3.Config = string(configJSON)
	installReq3.RuntimeID = mcpID
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq3); err != nil {
		// 回滚 mcp_servers
		h.ExtRepo.DeleteMCPServer(r.Context(), mcpID) //nolint:errcheck
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if h.MCPMgr != nil {
		//nolint:errcheck
		go h.StartMCPServer(protocol.Detach(r.Context()), types.MCPServerConfig{
			ID:        mcpID,
			Name:      req.Name,
			Transport: req.Transport,
			Command:   req.Command,
			Args:      req.Args,
			Env:       req.Env,
			URL:       req.URL,
			Timeout:   30,
			TrustTier: 1,
			Enabled:   true,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "mcp_id": mcpID, "name": req.Name, "type": "mcp",
	})
}

// HandleCreatePluginFromIntent intent 模式实现：LLM 生成 MCP 插件并注册为本地 MCP Server。
// 由 HandleCreatePlugin 在确认 pluginCreator 非空且 intent 非空后调用。
// 函数内部负责全部响应写入；调用方收到返回后直接 return。
func (h *PluginHandler) HandleCreatePluginFromIntent( //nolint:cyclop
	w http.ResponseWriter, r *http.Request,
	extID string, installReq marketplace.InstallRequest, intent string,
) {
	pluginDir, err := h.PluginCreator.GeneratePlugin(r.Context(), intent, 1 /* TrustLocal */)
	if err != nil {
		http.Error(w, "plugin_creator: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 解析 .mcp.json 取得运行时 command/args
	mcpJSONRaw, err := os.ReadFile(filepath.Join(pluginDir, ".mcp.json"))
	if err != nil {
		http.Error(w, "plugin_creator: read .mcp.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var mcpFileCfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpJSONRaw, &mcpFileCfg); err != nil {
		http.Error(w, "plugin_creator: parse .mcp.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// .mcp.json 中只有一个 server entry，取第一个
	var genCmd string
	var genArgs []string
	for _, srv := range mcpFileCfg.MCPServers {
		genCmd = srv.Command
		genArgs = srv.Args
		break
	}
	pluginName := filepath.Base(pluginDir) // GeneratePlugin 以 result.Name 为目录名

	now := time.Now().UTC().Format(time.RFC3339)
	mcpID := util.GenerateHumanReadableID("mcp", installReq.Name)
	genArgsBytes, _ := json.Marshal(genArgs)
	genConfigJSON, _ := json.Marshal(map[string]any{
		"transport":  "stdio",
		"command":    genCmd,
		"args":       genArgs,
		"plugin_dir": pluginDir,
	})

	// 写 mcp_servers（运行时表）
	if err := h.ExtRepo.UpsertMCPServer(r.Context(), apptypes.MCPServerRow{
		ID:        mcpID,
		Name:      pluginName,
		Transport: "stdio",
		Command:   genCmd,
		Args:      string(genArgsBytes),
		Env:       "{}",
		URL:       "",
		Enabled:   true,
		Timeout:   30,
		TrustTier: 1,
		CatalogID: "",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		http.Error(w, "mcp_servers insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 写 extension_instances（安装 SSoT）
	installReq.Name = pluginName
	installReq.ExtType = "mcp" // 生成后本质是 MCP Server
	installReq.Config = string(genConfigJSON)
	installReq.RuntimeID = mcpID
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq); err != nil {
		_ = h.ExtRepo.DeleteMCPServer(r.Context(), mcpID)
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 异步启动 MCP Server
	if h.MCPMgr != nil {
		//nolint:errcheck
		go h.StartMCPServer(protocol.Detach(r.Context()), types.MCPServerConfig{
			ID:        mcpID,
			Name:      pluginName,
			Transport: "stdio",
			Command:   genCmd,
			Args:      genArgs,
			Timeout:   30,
			TrustTier: 1,
			Enabled:   true,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "mcp_id": mcpID, "name": pluginName,
		"type": "mcp", "plugin_dir": pluginDir,
	})
}
