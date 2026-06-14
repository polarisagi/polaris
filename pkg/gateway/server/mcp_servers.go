package server

import (
	"context"
	"fmt"

	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/extensions/marketplace"
	"github.com/polarisagi/polaris/pkg/extensions/mcp"
)

// MCPServerConfig MCP Server REST API 数据结构。
type MCPServerConfig struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // "stdio" | "sse" | "streamable_http"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Enabled   bool              `json:"enabled"`
	Timeout   int               `json:"timeout"` // 秒
	// TrustTier 信任级别（ADR-0016）：0=Untrusted,1=Local,2=Community,3=Official,4=System
	TrustTier int    `json:"trust_tier"`
	CatalogID string `json:"catalog_id,omitempty"`
	// PluginID 非空表示此 MCP 来自插件 bundle（plugins.id）；独立安装时为空。
	// 插件 MCP 不支持独立 DELETE/PUT，需通过 /v1/plugins/{id} 管理。
	PluginID  string `json:"plugin_id,omitempty"`
	WorkDir   string `json:"work_dir,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// 只读，运行时状态
	Connected  bool   `json:"connected,omitempty"`
	ToolCount  int    `json:"tool_count,omitempty"`
	Error      string `json:"error,omitempty"`
	PluginName string `json:"plugin_name,omitempty"` // JOIN plugins 取 display_name
}

func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	// 统一查询：独立安装的 MCP（plugin_id=''）和插件内嵌的 MCP（plugin_id!=''）都在 mcp_servers 表中。
	// LEFT JOIN plugins 取 display_name 用于前端展示插件来源。
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT ms.id, ms.name, ms.transport, ms.command, ms.args, ms.env, ms.url,
		       ms.enabled, ms.timeout, ms.trust_tier, COALESCE(ms.catalog_id,''),
		       ms.plugin_id, ms.work_dir, ms.created_at, ms.updated_at,
		       COALESCE(p.display_name, p.name, '') AS plugin_name
		FROM mcp_servers ms
		LEFT JOIN plugins p ON ms.plugin_id = p.id
		ORDER BY ms.created_at`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	runtimeMap := map[string]mcp.MCPServerInfo{}
	if s.mcpMgr != nil {
		for _, info := range s.mcpMgr.ListServers() {
			runtimeMap[info.ID] = info
		}
	}

	list := []*MCPServerConfig{}
	for rows.Next() {
		c := &MCPServerConfig{}
		var enabled int
		var argsJSON, envJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON,
			&c.URL, &enabled, &c.Timeout, &c.TrustTier, &c.CatalogID,
			&c.PluginID, &c.WorkDir, &c.CreatedAt, &c.UpdatedAt, &c.PluginName); err != nil {
			continue
		}
		c.Enabled = enabled == 1
		json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
		json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
		if info, ok := runtimeMap[c.ID]; ok {
			c.Connected = info.Connected
			c.ToolCount = len(info.Tools)
			c.Error = info.Error
		}
		list = append(list, c)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"mcp_servers": list}) //nolint:errcheck
}

func (s *Server) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	// PolicyGate 是安全门，不允许 nil 跳过（fail-closed）。
	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	var c MCPServerConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	authCtxM := FromContext(r.Context())
	principal := authCtxM.UserID
	if principal == "" {
		principal = "user"
	}
	installReq := marketplace.InstallRequest{
		Principal:   principal,
		ExtensionID: "mcp_pending",
		ExtType:     "mcp",
		TrustTier:   c.TrustTier,
		Publisher:   "user",
		HasHooks:    false,
	}
	if err := s.installMgr.Authorize(r.Context(), installReq); err != nil {
		http.Error(w, "policy denied: "+err.Error(), http.StatusForbidden)
		return
	}

	if err := s.installMgr.InstallExtension(r.Context(), installReq); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 生成 ID 并持久化到 mcp_servers（State-in-DB，重启可恢复）
	if c.ID == "" {
		c.ID = "mcp_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	argsBytes, _ := json.Marshal(c.Args)
	envBytes, _ := json.Marshal(c.Env)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO mcp_servers(id, name, transport, command, args, env, url, enabled, timeout, trust_tier, catalog_id, plugin_id, work_dir, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,'','','',?,?)`,
		c.ID, c.Name, c.Transport, c.Command,
		string(argsBytes), string(envBytes),
		c.URL, boolToInt(c.Enabled), c.Timeout, c.TrustTier, now, now)
	if err != nil {
		http.Error(w, "db: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if c.Enabled && s.mcpMgr != nil {
		go s.startMCPServer(protocol.Detach(r.Context()), c)
	}

	c.CreatedAt, c.UpdatedAt = now, now
	s.clearToolSchemaCache()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (s *Server) handleUpdateMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	// 插件 MCP 不允许独立更新——需通过 PUT /v1/plugins/{id} 管理
	var pluginID string
	_ = s.db.QueryRowContext(r.Context(), `SELECT plugin_id FROM mcp_servers WHERE id=?`, id).Scan(&pluginID)
	if pluginID != "" {
		http.Error(w, "plugin MCP cannot be updated independently; manage via PUT /v1/plugins/{id}", http.StatusMethodNotAllowed)
		return
	}
	var c MCPServerConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.installMgr == nil {
		http.Error(w, "install manager not initialized", http.StatusServiceUnavailable)
		return
	}
	authCtxM := FromContext(r.Context())
	principal := authCtxM.UserID
	if principal == "" {
		principal = "user"
	}
	if err := s.installMgr.Authorize(r.Context(), marketplace.InstallRequest{
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

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE mcp_servers SET name=?, transport=?, command=?, args=?, env=?, url=?, enabled=?, timeout=?, trust_tier=?, updated_at=? WHERE id=?`,
		c.Name, c.Transport, c.Command, string(argsBytes), string(envBytes),
		c.URL, boolToInt(c.Enabled), c.Timeout, c.TrustTier, now, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if s.mcpMgr != nil {
		s.mcpMgr.Remove(id)
		if c.Enabled {
			c.ID = id
			go s.startMCPServer(protocol.Detach(r.Context()), c)
		}
	}

	c.ID = id
	c.UpdatedAt = now
	s.clearToolSchemaCache()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	// 插件 MCP 不允许独立删除——需通过 DELETE /v1/plugins/{catalogID} 卸载整个插件
	var pluginID string
	_ = s.db.QueryRowContext(r.Context(), `SELECT plugin_id FROM mcp_servers WHERE id=?`, id).Scan(&pluginID)
	if pluginID != "" {
		http.Error(w, "plugin MCP cannot be deleted independently; uninstall the plugin via DELETE /v1/plugins/{catalogID}", http.StatusMethodNotAllowed)
		return
	}
	if s.mcpMgr != nil {
		s.mcpMgr.Remove(id)
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM mcp_servers WHERE id=?`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.clearToolSchemaCache()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"}) //nolint:errcheck
}

// handleTestMCPServer 测试连接指定 MCP Server，返回连接状态和工具数量。
func (s *Server) handleTestMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	if s.mcpMgr == nil {
		http.Error(w, "mcp manager not initialized", http.StatusServiceUnavailable)
		return
	}

	// 从数据库读取配置
	var c MCPServerConfig
	var argsJSON, envJSON string
	row := s.db.QueryRowContext(r.Context(),
		`SELECT name, transport, command, args, env, url, timeout FROM mcp_servers WHERE id=?`, id)
	if err := row.Scan(&c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON, &c.URL, &c.Timeout); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
	json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
	c.ID = id

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.startMCPServerCtx(ctx, c); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()}) //nolint:errcheck
		return
	}

	toolCount := 0
	for _, info := range s.mcpMgr.ListServers() {
		if info.ID == id {
			toolCount = len(info.Tools)
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "tool_count": toolCount}) //nolint:errcheck
}

// startMCPServer 异步连接 MCP Server（新建/更新时 goroutine 调用）。
func (s *Server) startMCPServer(ctx context.Context, c MCPServerConfig) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := s.startMCPServerCtx(ctx, c); err != nil {
		slog.Warn("mcp: connect server failed", "id", c.ID, "err", err)
	}
}

func (s *Server) startMCPServerCtx(ctx context.Context, c MCPServerConfig) error {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = strings.ReplaceAll(a, "{DATA_DIR}", s.dataDir)
	}
	cfg := mcp.MCPClientConfig{
		Transport:  mcp.MCPTransport(c.Transport),
		Command:    c.Command,
		Args:       args,
		Env:        c.Env,
		URL:        strings.ReplaceAll(c.URL, "{DATA_DIR}", s.dataDir),
		WorkDir:    c.WorkDir, // plugin MCP = install_path；独立 MCP = ""（继承父进程 cwd）
		Timeout:    time.Duration(c.Timeout) * time.Second,
		ServerName: c.Name,
	}
	return s.mcpMgr.Add(ctx, c.ID, c.Name, cfg)
}
