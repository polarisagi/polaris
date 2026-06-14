package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/extensions/mcp"
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

// handleListPlugins 返回已安装插件列表，含子 MCP 运行时状态。
// 子 MCP 状态从 mcp_servers 表读取（State-in-DB），不再解析 mcp_policy.enabled。
// GET /v1/plugins
func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, version, display_name, description, publisher, enabled,
		        trust_tier, mcp_policy, install_path, catalog_id, created_at, updated_at
		 FROM plugins ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	connectedMCPs := make(map[string]mcp.MCPServerInfo)
	if s.mcpMgr != nil {
		for _, srv := range s.mcpMgr.ListServers() {
			connectedMCPs[srv.ID] = srv
		}
	}

	var result []pluginResponse
	for rows.Next() {
		var p pluginRow
		var enabledInt int
		if err := rows.Scan(&p.ID, &p.Name, &p.Version, &p.DisplayName, &p.Description,
			&p.Publisher, &enabledInt, &p.TrustTier, &p.MCPPolicy, &p.InstallPath,
			&p.CatalogID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		p.Enabled = enabledInt == 1

		mcpStatuses := []pluginMCPStatus{}
		mcpRows, err2 := s.db.QueryContext(r.Context(),
			`SELECT id, name, enabled FROM mcp_servers WHERE plugin_id=? ORDER BY created_at`, p.ID)
		if err2 == nil {
			for mcpRows.Next() {
				var serverID, serverName string
				var srvEnabled int
				if mcpRows.Scan(&serverID, &serverName, &srvEnabled) == nil {
					st := pluginMCPStatus{ID: serverID, Name: serverName, Enabled: srvEnabled == 1}
					if info, ok := connectedMCPs[serverID]; ok {
						st.Connected = info.Connected
						st.ToolCount = len(info.Tools)
						st.Error = info.Error
					}
					mcpStatuses = append(mcpStatuses, st)
				}
			}
			mcpRows.Close()
		}

		result = append(result, pluginResponse{pluginRow: p, MCPServers: mcpStatuses})
	}

	if result == nil {
		result = []pluginResponse{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"plugins": result, "total": len(result)})
}

// handleUpdatePlugin 更新插件启用状态或 mcp_policy，并级联同步 mcp_servers / skills / MCPManager。
// PUT /v1/plugins/{id}
func (s *Server) handleUpdatePlugin(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var mcpPolicyJSON string
	var currentEnabled int
	if err := s.db.QueryRowContext(r.Context(),
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

	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE plugins SET enabled=?, mcp_policy=?, updated_at=? WHERE id=?`,
		newEnabled, newMCPPolicy, now, pluginID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if req.Enabled != nil && currentEnabled != newEnabled {
		if newEnabled == 0 {
			s.disablePluginComponents(r.Context(), pluginID, now)
		} else {
			s.enablePluginComponents(r.Context(), pluginID, now)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "updated", "id": pluginID})
}

// disablePluginComponents 停止插件的所有子 MCP，并将 skills 标记为 deprecated。
func (s *Server) disablePluginComponents(ctx context.Context, pluginID, now string) {
	if s.mcpMgr != nil {
		mcpRows, err := s.db.QueryContext(ctx, `SELECT id FROM mcp_servers WHERE plugin_id=?`, pluginID)
		if err == nil {
			for mcpRows.Next() {
				var serverID string
				if mcpRows.Scan(&serverID) == nil {
					s.mcpMgr.Remove(serverID)
				}
			}
			mcpRows.Close()
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE mcp_servers SET enabled=0, updated_at=? WHERE plugin_id=?`, now, pluginID)
	_, _ = s.db.ExecContext(ctx, `UPDATE skills SET deprecated=1, updated_at=? WHERE plugin_id=?`, now, pluginID)
	s.clearToolSchemaCache()
}

// enablePluginComponents 启动插件的所有子 MCP，并恢复 skills。
func (s *Server) enablePluginComponents(ctx context.Context, pluginID, now string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE mcp_servers SET enabled=1, updated_at=? WHERE plugin_id=?`, now, pluginID)
	_, _ = s.db.ExecContext(ctx, `UPDATE skills SET deprecated=0, updated_at=? WHERE plugin_id=?`, now, pluginID)

	if s.mcpMgr != nil {
		mcpRows, err := s.db.QueryContext(ctx,
			`SELECT id, name, transport, command, args, env, url, timeout, work_dir
			 FROM mcp_servers WHERE plugin_id=? AND enabled=1`, pluginID)
		if err == nil {
			for mcpRows.Next() {
				var c MCPServerConfig
				var argsJSON, envJSON string
				if mcpRows.Scan(&c.ID, &c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON,
					&c.URL, &c.Timeout, &c.WorkDir) == nil {
					json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
					json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
					go s.startMCPServer(protocol.Detach(ctx), c)
				}
			}
			mcpRows.Close()
		}
	}
	s.clearToolSchemaCache()
}

// handleTogglePluginMCP 切换插件内单个子 MCP 的启用状态。
// 直接操作 mcp_servers.enabled（权威来源），不再通过 mcp_policy.enabled。
// PATCH /v1/plugins/{id}/mcp/{serverName}
func (s *Server) handleTogglePluginMCP(w http.ResponseWriter, r *http.Request) {
	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	if err := s.installMgr.AuthorizeAction(r.Context(), "plugin:manage", nil); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var exists int
	if s.db.QueryRowContext(r.Context(), `SELECT 1 FROM plugins WHERE id=? AND enabled=1`, pluginID).Scan(&exists) != nil {
		http.Error(w, "plugin not found or disabled", http.StatusNotFound)
		return
	}

	serverID := fmt.Sprintf("plugin_%s_%s", pluginID, serverName)
	now := time.Now().UTC().Format(time.RFC3339)

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE mcp_servers SET enabled=?, updated_at=? WHERE id=? AND plugin_id=?`,
		boolToInt(req.Enabled), now, serverID, pluginID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "MCP server not found for this plugin", http.StatusNotFound)
		return
	}

	if s.mcpMgr != nil {
		if !req.Enabled {
			s.mcpMgr.Remove(serverID)
		} else {
			var c MCPServerConfig
			var argsJSON, envJSON string
			row := s.db.QueryRowContext(r.Context(),
				`SELECT id, name, transport, command, args, env, url, timeout, work_dir
				 FROM mcp_servers WHERE id=?`, serverID)
			if row.Scan(&c.ID, &c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON,
				&c.URL, &c.Timeout, &c.WorkDir) == nil {
				json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
				json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
				go s.startMCPServer(protocol.Detach(r.Context()), c)
			}
		}
	}
	s.clearToolSchemaCache()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "updated",
		"plugin_id": pluginID,
		"server":    serverName,
		"enabled":   req.Enabled,
	})
}
