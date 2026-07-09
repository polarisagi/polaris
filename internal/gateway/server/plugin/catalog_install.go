package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"encoding/json"
	"maps"
	"net/http"

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
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.internalInstallMCP", err)
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
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.internalInstallMCP", err)
	}

	if h.MCPMgr != nil {
		concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.start_mcp_server_install", func(ctx context.Context) {
			_ = h.StartMCPServer(ctx, cfg)
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
		http.Error(w, "mcp_servers insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// internalInstallGeneric 安装 skill / plugin / app：写 extension_instances。
// skill/plugin 需异步下载文件并写运行时表（TODO: downloadAndInstall goroutine）。
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
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.internalInstallGeneric", err)
	}

	if entry.Type == "skill" || entry.Type == "plugin" {
		concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.download_and_install_extension", func(ctx context.Context) {
			h.downloadAndInstallExtension(ctx, extID, req.CatalogID, entry, now, name)
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
		http.Error(w, "extension_instances insert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// downloadAndInstallExtension（skill/plugin 异步下载安装）、
// updateExtensionInstanceError、copyDir/copyFile、pluginMCPDef 见
// catalog_download.go（R7 拆分）。registerPluginMCPServers 定义见
// catalog_register.go（其文档注释此前误留在本文件末尾，已随本次拆分归位）。
