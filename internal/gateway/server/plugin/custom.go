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
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// HandleCreateApp/HandleCreateMCP/HandleCreatePluginFromIntent 见 custom_app_mcp.go（R7 拆分）。

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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
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
	installReq0 := protocol.ExtensionInstallRequest{
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
				concurrent.SafeGo(bgCtx, "gateway.plugin.hitl_install_skill", func(bgCtx context.Context) {
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

	configJSON, _ := json.Marshal(map[string]any{
		"repo_url":   req.RepoURL,
		"entrypoint": req.Entrypoint,
	})

	installReq0.Name = req.Name
	installReq0.Config = string(configJSON)
	if err := h.InstallMgr.InstallExtension(r.Context(), installReq0); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
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
	installReq1 := protocol.ExtensionInstallRequest{
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
				concurrent.SafeGo(bgCtx, "gateway.plugin.hitl_install_plugin", func(bgCtx context.Context) {
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
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": extID, "name": req.Name, "type": "plugin",
	})
}
