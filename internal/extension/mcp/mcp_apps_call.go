package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CallToolAsApp 执行 MCP Apps View 发起的工具调用（POST /v1/mcp-apps/views/{id}/rpc
// 的工具调用方法）。与模型发起的调用（CallTool）走同一 ExecEnvelope.Execute 入口
// （HE-3），仅 Principal/AppViewID/AppSessionID 不同，供 PolicyGate 与审计区分来源。
//
// 服务器归属天然隔离：只在 serverID 对应 entry 的 tools 列表内查找，跨服务器调用
// 无法通过——调用方（网关 handler）负责校验 view 所属 serverID 与请求一致
// （apps_spec.mdx §Visibility "Cross-server tool calls are always blocked"）。
// 可见性校验：仅 visibility 含 "app" 的工具可被本方法调用（含 app-only 工具）。
//
// 返回值为原始 CallToolResult JSON（content/structuredContent/_meta/isError），
// 不进入模型上下文——调用方直接把它作为 JSON-RPC 响应 result 转发给 View。
func (m *MCPManager) CallToolAsApp(ctx context.Context, serverID, toolName string, args map[string]any, sessionID, viewID string) (json.RawMessage, error) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	reg := m.toolReg
	m.mu.RUnlock()
	if !ok || e.client == nil {
		return nil, apperr.New(apperr.CodeNotFound, "mcp apps: server not connected: "+serverID)
	}
	tool, found := findMCPToolByName(e.tools, toolName)
	if !found {
		return nil, apperr.New(apperr.CodeNotFound, "mcp apps: tool not found: "+toolName)
	}
	if !ToolUIVisibleTo(tool.UI, "app") {
		return nil, apperr.New(apperr.CodeForbidden, "mcp apps: tool not callable by app: "+toolName)
	}
	if reg == nil {
		return nil, apperr.New(apperr.CodeInternal, "mcp_manager: tool registry not initialized")
	}
	argsBytes, err := json.Marshal(args)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp apps: marshal tool input", err)
	}

	start := time.Now()
	// View 的参数来自服务器下发的 HTML 里运行的脚本，与工具输出同属外部内容，按 Medium 起算。
	res, execErr := reg.ExecuteAppTool(ctx, MCPToolName(e.name, toolName), argsBytes, types.TaintMedium, viewID, sessionID)
	outcome := "success"
	if execErr != nil || res == nil || !res.Success {
		outcome = "error"
	}
	metrics.RecordMCPAppToolCall(ctx, e.name, outcome)
	slog.Info("mcp apps: app tool call", "server", e.name, "tool", toolName, "view_id", viewID,
		"session_id", sessionID, "outcome", outcome, "latency_ms", time.Since(start).Milliseconds())

	if execErr != nil {
		return nil, apperr.Wrap(apperr.CodeOf(execErr), "mcp apps: call tool "+toolName, execErr)
	}
	if !res.Success {
		return nil, apperr.New(apperr.CodeInternal, "mcp apps: call tool failed: "+res.Error)
	}
	if len(res.MCPRaw) > 0 {
		return res.MCPRaw, nil
	}
	// 兜底：MCP 来源工具必有 MCPRaw；此分支防御未来非 MCP 来源工具被误标 app 可见，避免 View 解析空响应。
	fallback, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(res.Output)}},
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp apps: encode fallback result", err)
	}
	return fallback, nil
}

// ReadResourceAsApp 执行 MCP Apps View 发起的资源读取（POST .../rpc 的资源读取
// 方法）。仅在 serverID 对应连接内查找——资源命名空间天然按服务器隔离，调用方
// 无需（也无法）跨服务器读取。
func (m *MCPManager) ReadResourceAsApp(ctx context.Context, serverID, uri string) ([]protocol.MCPResourceContent, error) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok || e.client == nil {
		return nil, apperr.New(apperr.CodeNotFound, "mcp apps: server not connected: "+serverID)
	}
	contents, err := e.client.ResourcesRead(ctx, uri)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp apps: resources/read "+uri, err)
	}
	return contents, nil
}

// findMCPToolByName 在 entry 已缓存的工具列表中按 MCP 协议原始名（非 LLM 调用名）查找。
func findMCPToolByName(tools []MCPTool, name string) (MCPTool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return MCPTool{}, false
}
