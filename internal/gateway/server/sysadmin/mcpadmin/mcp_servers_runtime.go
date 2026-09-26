package mcpadmin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HandleTestMCPServer 测试连接指定 MCP Server，返回连接状态和工具数量。
func (h *MCPAdmin) HandleTestMCPServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("serverID")
	if h.MCPMgr == nil {
		http.Error(w, "mcp manager not initialized", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := h.MCPMgr.StartFromDB(ctx, id); err != nil {
		if apperr.IsCode(err, apperr.CodeNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
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

// startMCPServer 异步连接 MCP Server（新建/更新时 goroutine 调用）。配置以 mcp_servers 行为准。
func (h *MCPAdmin) startMCPServer(ctx context.Context, serverID string) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := h.MCPMgr.StartFromDB(ctx, serverID); err != nil {
		slog.Warn("mcp: connect server failed", "id", serverID, "err", err)
	}
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
		httputil.RespondError(w, "", err, statusCode)
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
