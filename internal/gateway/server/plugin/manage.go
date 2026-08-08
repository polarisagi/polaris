package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/types"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

type pluginRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Publisher   string `json:"publisher"`
	Enabled     bool   `json:"enabled"`
	TrustTier   int    `json:"trust_tier"`
	MCPPolicy   string `json:"mcp_policy"`
	InstallPath string `json:"install_path"`
	CatalogID   string `json:"catalog_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type pluginMCPStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	ToolCount int    `json:"tool_count"`
	Error     string `json:"error,omitempty"`
}

type pluginResponse struct {
	pluginRow
	MCPServers []pluginMCPStatus `json:"mcp_servers"`
}

// HandleListPlugins 返回已安装插件列表，含子 MCP 运行时状态。
// 子 MCP 状态从 mcp_servers 表读取（State-in-DB），不再解析 mcp_policy.enabled。
// GET /v1/plugins
//
// 两段式读取（2026-08-09）：先把 plugins 全部读进内存并关闭 rows，再一次性拉取
// mcp_servers 按 plugin_id 归组。原实现在 rows.Next() 循环体内对每行再发一次查询，
// 是「外层 Rows 未关闭时嵌套查询」——该模式会占住一条连接不放：readDB 池
// MaxOpenConns=4，第 4 个并发 List 请求到达时四条连接全被外层 Rows 占用，内层查询
// 无连接可用且外层要等内层完成，整个读池死锁；:memory: 测试库只有一条连接，
// 有任意一行数据即必然卡死（本函数正是被这条路径揪出来的）。
// 顺带消掉 N+1。
func (h *PluginHandler) HandleListPlugins(w http.ResponseWriter, r *http.Request) {
	plugins, err := h.listPluginRows(r)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	connectedMCPs := make(map[string]protocol.MCPServerInfo)
	if h.MCPMgr != nil {
		for _, srv := range h.MCPMgr.ListServers() {
			connectedMCPs[srv.ID] = srv
		}
	}
	mcpByPlugin := h.listPluginMCPStatuses(r, connectedMCPs)

	result := make([]pluginResponse, 0, len(plugins))
	for _, p := range plugins {
		statuses := mcpByPlugin[p.ID]
		if statuses == nil {
			statuses = []pluginMCPStatus{}
		}
		result = append(result, pluginResponse{pluginRow: p, MCPServers: statuses})
	}

	httputil.WriteJSON(w, map[string]any{"plugins": result, "total": len(result)})
}

// listPluginRows 读出全部插件行；返回前 rows 已关闭，调用方可安全发起后续查询。
func (h *PluginHandler) listPluginRows(r *http.Request) ([]pluginRow, error) {
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT id, name, version, display_name, description, publisher, enabled,
		        trust_tier, mcp_policy, install_path, catalog_id, created_at, updated_at
		 FROM plugins ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "plugin: 查询 plugins 列表失败", err)
	}
	defer rows.Close()

	var out []pluginRow
	for rows.Next() {
		var p pluginRow
		var enabledInt int
		if err := rows.Scan(&p.ID, &p.Name, &p.Version, &p.DisplayName, &p.Description,
			&p.Publisher, &enabledInt, &p.TrustTier, &p.MCPPolicy, &p.InstallPath,
			&p.CatalogID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		p.Enabled = enabledInt == 1
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "plugin: 遍历 plugins 结果集失败", err)
	}
	return out, nil
}

// listPluginMCPStatuses 一次性拉取全部子 MCP 并按 plugin_id 归组。
// 查询失败按空结果降级：子 MCP 状态是列表页的附加信息，不该让整个插件列表失败。
func (h *PluginHandler) listPluginMCPStatuses(
	r *http.Request, connected map[string]protocol.MCPServerInfo,
) map[string][]pluginMCPStatus {
	out := map[string][]pluginMCPStatus{}
	rows, err := h.DB.QueryContext(r.Context(),
		`SELECT plugin_id, id, name, enabled FROM mcp_servers
		 WHERE plugin_id IS NOT NULL AND plugin_id != '' ORDER BY created_at`)
	if err != nil {
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var pluginID, serverID, serverName string
		var srvEnabled int
		if rows.Scan(&pluginID, &serverID, &serverName, &srvEnabled) != nil {
			continue
		}
		st := pluginMCPStatus{ID: serverID, Name: serverName, Enabled: srvEnabled == 1}
		if info, ok := connected[serverID]; ok {
			st.Connected = info.Connected
			st.ToolCount = len(info.Tools)
			st.Error = info.Error
		}
		out[pluginID] = append(out[pluginID], st)
	}
	return out
}

// HandleUpdatePlugin 更新插件启用状态或 mcp_policy，并级联同步 mcp_servers / skills / MCPManager。
// PUT /v1/plugins/{id}
func (h *PluginHandler) HandleUpdatePlugin(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("id")
	if pluginID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	var req struct {
		Enabled   *bool                     `json:"enabled"`
		MCPPolicy map[string]map[string]any `json:"mcp_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	var mcpPolicyJSON string
	var currentEnabled int
	if err := h.DB.QueryRowContext(r.Context(),
		`SELECT enabled, mcp_policy FROM plugins WHERE id=?`, pluginID).
		Scan(&currentEnabled, &mcpPolicyJSON); err != nil {
		http.Error(w, "plugin not found", http.StatusNotFound)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	newEnabled := currentEnabled
	if req.Enabled != nil {
		if *req.Enabled {
			newEnabled = 1
		} else {
			newEnabled = 0
		}
	}

	newMCPPolicy := mcpPolicyJSON
	if req.MCPPolicy != nil {
		b, _ := json.Marshal(req.MCPPolicy)
		newMCPPolicy = string(b)
	}

	if err := h.ExtRepo.UpdatePluginStatus(r.Context(), pluginID, newEnabled, newMCPPolicy, now); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	if req.Enabled != nil && currentEnabled != newEnabled {
		if newEnabled == 0 {
			h.disablePluginComponents(r.Context(), pluginID, now)
		} else {
			h.enablePluginComponents(r.Context(), pluginID, now)
		}
	}

	httputil.WriteJSON(w, map[string]any{"status": "updated", "id": pluginID})
}

// disablePluginComponents 停止插件的所有子 MCP，并将 skills 标记为 deprecated。
func (h *PluginHandler) disablePluginComponents(ctx context.Context, pluginID, now string) {
	if h.MCPMgr != nil {
		mcpRows, err := h.DB.QueryContext(ctx, `SELECT id FROM mcp_servers WHERE plugin_id=?`, pluginID)
		if err == nil {
			for mcpRows.Next() {
				var serverID string
				if mcpRows.Scan(&serverID) == nil {
					h.MCPMgr.Remove(serverID)
				}
			}
			mcpRows.Close()
		}
	}
	if err := h.ExtRepo.SetPluginComponentsEnabled(ctx, pluginID, 0, now); err != nil {
		slog.Warn("plugin_manage: disable plugin components failed", "plugin", pluginID, "err", err)
	}
	h.ClearToolSchemaCache()
}

// enablePluginComponents 启动插件的所有子 MCP，并恢复 skills。
func (h *PluginHandler) enablePluginComponents(ctx context.Context, pluginID, now string) { //nolint:nestif
	if err := h.ExtRepo.SetPluginComponentsEnabled(ctx, pluginID, 1, now); err != nil {
		slog.Warn("plugin_manage: enable plugin components failed", "plugin", pluginID, "err", err)
	}

	if h.MCPMgr != nil { //nolint:nestif
		mcpRows, err := h.DB.QueryContext(ctx,
			`SELECT id, name, transport, command, args, env, url, timeout, work_dir, trust_tier
			 FROM mcp_servers WHERE plugin_id=? AND enabled=1`, pluginID)
		if err == nil {
			for mcpRows.Next() {
				var c types.MCPServerConfig
				var argsJSON, envJSON string
				if mcpRows.Scan(&c.ID, &c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON,
					&c.URL, &c.Timeout, &c.WorkDir, &c.TrustTier) == nil {
					json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
					json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
					concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.start_mcp_server_enable", func(ctx context.Context) {
						if err := h.StartMCPServer(ctx, c); err != nil {
							slog.Warn("plugin_manage: start mcp server on enable failed", "id", c.ID, "err", err)
						}
					})
				}
			}
			mcpRows.Close()
		}
	}
	h.ClearToolSchemaCache()
}

// HandleTogglePluginMCP 切换插件内单个子 MCP 的启用状态。
// 直接操作 mcp_servers.enabled（权威来源），不再通过 mcp_policy.enabled。
// PATCH /v1/plugins/{id}/mcp/{serverName}
func (h *PluginHandler) HandleTogglePluginMCP(w http.ResponseWriter, r *http.Request) { //nolint:nestif
	if h.InstallMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtx := authcontext.FromContext(r.Context())
	principal := authCtx.UserID
	if principal == "" {
		principal = "user"
	}
	if err := h.InstallMgr.AuthorizeAction(r.Context(), principal, "plugin:manage", nil); err != nil {
		httputil.RespondError(w, "", err, http.StatusForbidden)
		return
	}

	pluginID := r.PathValue("id")
	serverName := r.PathValue("serverName")
	if pluginID == "" || serverName == "" {
		http.Error(w, "id and serverName required", http.StatusBadRequest)
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}

	var exists int
	if h.DB.QueryRowContext(r.Context(), `SELECT 1 FROM plugins WHERE id=? AND enabled=1`, pluginID).Scan(&exists) != nil {
		http.Error(w, "plugin not found or disabled", http.StatusNotFound)
		return
	}

	serverID := fmt.Sprintf("plugin_%s_%s", pluginID, serverName)
	now := time.Now().UTC().Format(time.RFC3339)

	err := h.ExtRepo.UpdatePluginMCPServerEnabled(r.Context(), pluginID, serverID, boolToInt(req.Enabled), now)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	if h.MCPMgr != nil { //nolint:nestif
		if !req.Enabled {
			h.MCPMgr.Remove(serverID)
		} else {
			var c types.MCPServerConfig
			var argsJSON, envJSON string
			row := h.DB.QueryRowContext(r.Context(),
				`SELECT id, name, transport, command, args, env, url, timeout, work_dir, trust_tier
				 FROM mcp_servers WHERE id=?`, serverID)
			if row.Scan(&c.ID, &c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON,
				&c.URL, &c.Timeout, &c.WorkDir, &c.TrustTier) == nil {
				json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
				json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
				concurrent.SafeGo(protocol.Detach(r.Context()), "gateway.plugin.start_mcp_server_toggle", func(ctx context.Context) {
					if err := h.StartMCPServer(ctx, c); err != nil {
						slog.Warn("plugin_manage: start mcp server on toggle failed", "id", c.ID, "err", err)
					}
				})
			}
		}
	}
	h.ClearToolSchemaCache()

	httputil.WriteJSON(w, map[string]any{
		"status":    "updated",
		"plugin_id": pluginID,
		"server":    serverName,
		"enabled":   req.Enabled,
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
