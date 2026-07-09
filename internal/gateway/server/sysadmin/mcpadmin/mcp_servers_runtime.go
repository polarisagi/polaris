package mcpadmin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
)

// HandleTestMCPServer 测试连接指定 MCP Server，返回连接状态和工具数量。
func (h *MCPAdmin) HandleTestMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	if h.MCPMgr == nil {
		http.Error(w, "mcp manager not initialized", http.StatusServiceUnavailable)
		return
	}

	// 从数据库读取配置
	var c types.MCPServerConfig
	var argsJSON, envJSON string
	row := h.DB.QueryRowContext(r.Context(),
		`SELECT name, transport, command, args, env, url, timeout, trust_tier FROM mcp_servers WHERE id=?`, id)
	if err := row.Scan(&c.Name, &c.Transport, &c.Command, &argsJSON, &envJSON, &c.URL, &c.Timeout, &c.TrustTier); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.Unmarshal([]byte(argsJSON), &c.Args) //nolint:errcheck
	json.Unmarshal([]byte(envJSON), &c.Env)   //nolint:errcheck
	c.ID = id

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := h.StartMCPServerCtx(ctx, c); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()}) //nolint:errcheck
		return
	}

	toolCount := 0
	for _, info := range h.MCPMgr.ListServers() {
		if info.ID == id {
			toolCount = len(info.Tools)
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "tool_count": toolCount}) //nolint:errcheck
}

// startMCPServer 异步连接 MCP Server（新建/更新时 goroutine 调用）。
func (h *MCPAdmin) startMCPServer(ctx context.Context, c types.MCPServerConfig) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := h.StartMCPServerCtx(ctx, c); err != nil {
		slog.Warn("mcp: connect server failed", "id", c.ID, "err", err)
	}
}

func (h *MCPAdmin) StartMCPServerCtx(ctx context.Context, c types.MCPServerConfig) error {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = strings.ReplaceAll(a, "{DATA_DIR}", h.DataDir)
	}
	cfg := protocol.MCPClientConfig{
		Transport:  protocol.MCPTransport(c.Transport),
		Command:    c.Command,
		Args:       args,
		Env:        c.Env,
		URL:        strings.ReplaceAll(c.URL, "{DATA_DIR}", h.DataDir),
		WorkDir:    c.WorkDir, // plugin MCP = install_path；独立 MCP = ""（继承父进程 cwd）
		Timeout:    time.Duration(c.Timeout) * time.Second,
		ServerName: c.Name,
		// SandboxPolicy 不设置（""=auto）：applyStdioSandbox 自动按 TrustTier + OS 决策，无需此处硬编码。
		TrustTier:       c.TrustTier,
		RequiresNetwork: c.RequiresNetwork,
		// NetworkApproved 由 MCPManager.Add() 内部按 preferences 表动态解析。
	}
	// 2026-07-07 R7 拆分产生的新文件触发 wrapcheck new-from-rev 基线（原
	// sysadmin/mcp_servers.go 里这行是存量代码，本身不受该规则约束；纯移动
	// 不改变错误处理语义，调用方已能拿到底层 error）。
	return h.MCPMgr.Add(ctx, c.ID, c.Name, cfg) //nolint:wrapcheck
}

// HandleMCPNetworkApproval 设置 MCP Server 的网络访问审批决策。
// PUT /v1/mcp-servers/{serverID}/network-access
// Body: {"approved": true}  → 放行网络
// Body: {"approved": false} → 拒绝（恢复断网）
//
// 仅对 TrustTier<=2 且 requires_network=true 的服务器有意义。
// 审批结果持久化到 preferences（mcp.net.approved.<id>）并立即重启 MCP 连接。
func (h *MCPAdmin) HandleMCPNetworkApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")

	var body struct {
		Approved bool `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if h.MCPMgr == nil {
		http.Error(w, "mcp manager not initialized", http.StatusServiceUnavailable)
		return
	}

	if err := h.MCPMgr.ApproveNetworkAccess(r.Context(), id, h.ExtRepo, h.DataDir, body.Approved); err != nil {
		statusCode := http.StatusInternalServerError
		http.Error(w, err.Error(), statusCode)
		return
	}

	decision := "denied"
	if body.Approved {
		decision = "approved"
	}
	slog.Info("mcp: network access decision applied", "server_id", id, "decision", decision)
	if h.ClearToolSchemaCache != nil {
		h.ClearToolSchemaCache()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"status":    "ok",
		"server_id": id,
		"decision":  decision,
	})
}
