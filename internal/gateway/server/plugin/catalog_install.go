package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

func (h *PluginHandler) internalInstallMCP(ctx context.Context, extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string, bypassAuth bool) (any, error) {
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

	installReq := protocol.ExtensionInstallRequest{
		Principal:   "system", // Auth is already checked in HandleInstallPlugin
		ExtensionID: extID,
		CatalogID:   req.CatalogID,
		Name:        cfg.Name,
		ExtType:     "mcp",
		TrustTier:   entry.TrustTier,
		Publisher:   entry.Publisher,
		Config:      string(configJSON),
		RuntimeID:   mcpID,
		BypassAuth:  bypassAuth,
	}

	if err := h.InstallMgr.InstallExtension(ctx, installReq); err != nil {
		// [2026-08-02 S-06 抽样复查] InstallExtension 内部 PolicyGate 拒绝时返回
		// CodeForbidden，此前恒被 CodeInternal 覆盖，导致客户端本应看到 403 却
		// 收到 500。改用 apperr.CodeOf(err) 保留内层真实语义 Code。
		return nil, apperr.Wrap(apperr.CodeOf(err), "Server.internalInstallMCP", err)
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
		if delErr := h.ExtRepo.DeleteInstance(ctx, extID); delErr != nil {
			slog.Warn("plugin_catalog: rollback extension_instances row failed", "ext", extID, "err", delErr)
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.internalInstallMCP", err)
	}

	if h.MCPMgr != nil {
		concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.start_mcp_server_install", func(ctx context.Context) {
			if err := h.StartMCPServer(ctx, mcpID); err != nil {
				slog.Warn("plugin_catalog: start mcp server on install failed", "id", cfg.ID, "err", err)
			}
		})
	}

	cfg.CreatedAt, cfg.UpdatedAt = now, now
	return map[string]any{
		"id":         extID,
		"type":       "mcp",
		"server":     cfg,
		"catalog_id": req.CatalogID,
	}, nil
}

// installMCPExtension 安装 MCP 类型：写 extension_instances + mcp_servers + 异步启动。
func (h *PluginHandler) installMCPExtension(w http.ResponseWriter, r *http.Request,
	extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string) {
	resp, err := h.internalInstallMCP(r.Context(), extID, entry, req, now, false)
	if err != nil {
		// GR-9.2-007：按底层 apperr 码映射状态（PolicyGate 拒绝 → 403、依赖缺失 → 404），
		// 不再一律 500 抹掉鉴权语义。
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSONStatus(w, http.StatusCreated, resp)
}

// internalInstallGeneric 安装 skill / plugin：写 extension_instances。
// skill/plugin 经 CatalogInstaller 异步取回来源（含依赖）并写运行时表。
func (h *PluginHandler) internalInstallGeneric(ctx context.Context, extID string, entry *protocol.RegistryEntry, req protocol.PluginInstallRequest, now string, bypassAuth bool) (any, error) {
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

	installReq := protocol.ExtensionInstallRequest{
		Principal:   "system",
		ExtensionID: extID,
		CatalogID:   req.CatalogID,
		Name:        name,
		ExtType:     entry.Type,
		TrustTier:   entry.TrustTier,
		Publisher:   entry.Publisher,
		Config:      string(configJSON),
		RuntimeID:   "",
		BypassAuth:  bypassAuth,
	}

	if err := h.InstallMgr.InstallExtension(ctx, installReq); err != nil {
		// [2026-08-02 S-06 抽样复查] 同 internalInstallMCP：保留 InstallExtension
		// 内层 PolicyGate 拒绝的真实 Code（CodeForbidden），不再被 CodeInternal 覆盖。
		return nil, apperr.Wrap(apperr.CodeOf(err), "Server.internalInstallGeneric", err)
	}

	if entry.Type == "skill" || entry.Type == "plugin" {
		if h.Catalog == nil {
			h.updateExtensionInstanceError(ctx, extID, "catalog installer not configured")
			return nil, apperr.New(apperr.CodeInternal, "Server.internalInstallGeneric: catalog installer not configured")
		}
		principal := installReq.Principal
		concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.catalog_install", func(ctx context.Context) {
			if _, err := h.Catalog.Install(ctx, marketplace.CatalogInstallRequest{CatalogID: req.CatalogID, ExtensionID: extID,
				Principal: principal, BypassAuth: bypassAuth}); err != nil {
				h.updateExtensionInstanceError(ctx, extID, err.Error())
			}
		})
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
	resp, err := h.internalInstallGeneric(r.Context(), extID, entry, req, now, false)
	if err != nil {
		// GR-9.2-007：按底层 apperr 码映射状态（PolicyGate 拒绝 → 403、依赖缺失 → 404），
		// 不再一律 500 抹掉鉴权语义。
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSONStatus(w, http.StatusCreated, resp)
}
