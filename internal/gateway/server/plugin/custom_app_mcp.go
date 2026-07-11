package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// HandleCreatePluginFromIntent 见 custom_plugin_intent.go（R7 二次拆分）。

// HandleCreateApp 用户手动创建 App 扩展（URL 模式）。
// POST /v1/apps/create
func (h *PluginHandler) HandleCreateApp(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
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
	installReq2 := protocol.ExtensionInstallRequest{
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
				concurrent.SafeGo(bgCtx, "gateway.plugin.hitl_install_app", func(bgCtx context.Context) {
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
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		httputil.RespondError(w, "Internal Server Error", err, http.StatusForbidden)
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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
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
	installReq3 := protocol.ExtensionInstallRequest{
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
				concurrent.SafeGo(bgCtx, "gateway.plugin.hitl_install_mcp", func(bgCtx context.Context) {
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
				})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending_approval", "id": extID})
				return
			}
		}
		httputil.RespondError(w, "Internal Server Error", err, http.StatusForbidden)
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
		concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.plugin.start_mcp_server", func(ctx context.Context) {
			_ = h.StartMCPServer(ctx, types.MCPServerConfig{
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
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "mcp_id": mcpID, "name": req.Name, "type": "mcp",
	})
}
