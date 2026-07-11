package mcpadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// types.MCPServerConfig MCP Server REST API 数据结构。

func (h *MCPAdmin) HandleListMCPServers(w http.ResponseWriter, r *http.Request) {
	// 统一查询：独立安装的 MCP（plugin_id=''）和插件内嵌的 MCP（plugin_id!=''）都在 mcp_servers 表中。
	// LEFT JOIN plugins 取 display_name 用于前端展示插件来源。
	rows, err := h.DB.QueryContext(r.Context(), `
		SELECT ms.id, ms.name, ms.transport, ms.command, ms.args, ms.env, ms.url,
		       ms.enabled, ms.timeout, ms.trust_tier, COALESCE(ms.catalog_id,''),
		       ms.plugin_id, ms.work_dir, ms.requires_network, ms.created_at, ms.updated_at,
		       COALESCE(p.display_name, p.name, '') AS plugin_name
		FROM mcp_servers ms
		LEFT JOIN plugins p ON ms.plugin_id = p.id
		ORDER BY ms.created_at`)
	if err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	runtimeMap := map[string]protocol.MCPServerInfo{}
	if h.MCPMgr != nil {
		for _, info := range h.MCPMgr.ListServers() {
			runtimeMap[info.ID] = info
		}
	}

	// 批量读取 preferences 中的网络审批状态（避免 N+1 查询）
	approvalMap := map[string]string{}
	if h.SystemRepo != nil {
		if prefs, err := h.SystemRepo.ListPreferences(r.Context()); err == nil {
			const prefix = "mcp.net.approved."
			for k, v := range prefs {
				if strings.HasPrefix(k, prefix) {
					approvalMap[k[len(prefix):]] = v
				}
			}
		}
	}

	list := []*types.MCPServerConfig{}
	for rows.Next() {
		c := &types.MCPServerConfig{}
		var enabled, requiresNetworkInt int
		var argsJSON, envJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON,
			&c.URL, &enabled, &c.Timeout, &c.TrustTier, &c.CatalogID,
			&c.PluginID, &c.WorkDir, &requiresNetworkInt, &c.CreatedAt, &c.UpdatedAt, &c.PluginName); err != nil {
			continue
		}
		c.Enabled = enabled == 1
		c.RequiresNetwork = requiresNetworkInt == 1
		json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
		json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
		if info, ok := runtimeMap[c.ID]; ok {
			c.Connected = info.Connected
			c.ToolCount = len(info.Tools)
			c.Error = info.Error
		}
		// 填充网络审批状态（仅对 TrustTier<=2 && RequiresNetwork=true 的服务器有意义）
		if c.RequiresNetwork && c.TrustTier <= 2 {
			switch approvalMap[c.ID] {
			case "approved":
				c.NetworkApprovalStatus = "approved"
			case "denied":
				c.NetworkApprovalStatus = "denied"
			default:
				c.NetworkApprovalStatus = "pending"
			}
		}
		list = append(list, c)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"mcp_servers": list}) //nolint:errcheck
}

func (h *MCPAdmin) HandleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	// PolicyGate 是安全门，不允许 nil 跳过（fail-closed）。
	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	var c types.MCPServerConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}

	authCtxM := authcontext.FromContext(r.Context())
	principal := authCtxM.UserID
	if principal == "" {
		principal = "user"
	}
	installReq := protocol.ExtensionInstallRequest{
		Principal:   principal,
		ExtensionID: "mcp_pending",
		ExtType:     "mcp",
		TrustTier:   c.TrustTier,
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := h.InstallMgr.Authorize(r.Context(), installReq); err != nil {
		http.Error(w, "policy denied: "+err.Error(), http.StatusForbidden)
		return
	}

	if err := h.InstallMgr.InstallExtension(r.Context(), installReq); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}

	// 生成 ID 并持久化到 mcp_servers（State-in-DB，重启可恢复）
	if c.ID == "" {
		c.ID = "mcp_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	argsBytes, _ := json.Marshal(c.Args)
	envBytes, _ := json.Marshal(c.Env)
	now := time.Now().UTC().Format(time.RFC3339)
	err := h.ExtRepo.UpsertMCPServer(r.Context(), apptypes.MCPServerRow{
		ID:              c.ID,
		Name:            c.Name,
		Transport:       c.Transport,
		Command:         c.Command,
		Args:            string(argsBytes),
		Env:             string(envBytes),
		URL:             c.URL,
		Enabled:         c.Enabled,
		Timeout:         c.Timeout,
		TrustTier:       c.TrustTier,
		CatalogID:       "",
		PluginID:        "",
		WorkDir:         "",
		RequiresNetwork: c.RequiresNetwork,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if c.Enabled && h.MCPMgr != nil {
		concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.sysadmin.start_mcp_server", func(ctx context.Context) {
			h.startMCPServer(ctx, c)
		})
	}

	c.CreatedAt, c.UpdatedAt = now, now
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (h *MCPAdmin) HandleUpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	// 插件 MCP 不允许独立更新——需通过 PUT /v1/plugins/{id} 管理
	serverRow, err := h.ExtRepo.GetMCPServer(r.Context(), id)
	if err != nil || serverRow == nil {
		if err == nil {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		}
		return
	}
	if serverRow.PluginID != "" {
		http.Error(w, "plugin MCP cannot be updated independently; manage via PUT /v1/plugins/{id}", http.StatusMethodNotAllowed)
		return
	}
	var c types.MCPServerConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusBadRequest)
		return
	}

	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtxM := authcontext.FromContext(r.Context())
	principal := authCtxM.UserID
	if principal == "" {
		principal = "user"
	}
	if err := h.InstallMgr.Authorize(r.Context(), protocol.ExtensionInstallRequest{
		Principal:   principal,
		ExtensionID: id,
		ExtType:     "mcp",
		TrustTier:   c.TrustTier,
		Publisher:   "user",
		HasHooks:    false,
	}); err != nil {
		http.Error(w, "policy denied: "+err.Error(), http.StatusForbidden)
		return
	}

	argsBytes, _ := json.Marshal(c.Args)
	envBytes, _ := json.Marshal(c.Env)
	now := time.Now().UTC().Format(time.RFC3339)

	if h.MCPMgr != nil {
		updateCfg := protocol.MCPUpdateConfig{
			Name:            c.Name,
			Transport:       c.Transport,
			Command:         c.Command,
			Args:            c.Args,
			Env:             c.Env,
			URL:             c.URL,
			Enabled:         c.Enabled,
			Timeout:         c.Timeout,
			TrustTier:       c.TrustTier,
			RequiresNetwork: c.RequiresNetwork,
		}
		if err := h.MCPMgr.Update(r.Context(), h.ExtRepo, id, updateCfg, h.DataDir); err != nil {
			httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
			return
		}
	} else {
		err := h.ExtRepo.UpdateMCPServer(r.Context(), id, map[string]any{
			"name":             c.Name,
			"transport":        c.Transport,
			"command":          c.Command,
			"args":             string(argsBytes),
			"env":              string(envBytes),
			"url":              c.URL,
			"enabled":          boolToInt(c.Enabled),
			"timeout":          c.Timeout,
			"trust_tier":       c.TrustTier,
			"requires_network": boolToInt(c.RequiresNetwork),
			"updated_at":       now,
		})
		if err != nil {
			httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
			return
		}
	}

	c.ID = id
	c.UpdatedAt = now
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (h *MCPAdmin) HandleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	// 插件 MCP 不允许独立删除——需通过 DELETE /v1/plugins/{catalogID} 卸载整个插件
	serverRow, err := h.ExtRepo.GetMCPServer(r.Context(), id)
	if err != nil || serverRow == nil {
		if err == nil {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		}
		return
	}
	if serverRow.PluginID != "" {
		http.Error(w, "plugin MCP cannot be deleted independently; uninstall the plugin via DELETE /v1/plugins/{catalogID}", http.StatusMethodNotAllowed)
		return
	}
	if h.MCPMgr != nil {
		h.MCPMgr.Remove(id)
	}
	if err := h.ExtRepo.DeleteMCPServer(r.Context(), id); err != nil {
		httputil.RespondError(w, "Internal Server Error", err, http.StatusInternalServerError)
		return
	}
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
