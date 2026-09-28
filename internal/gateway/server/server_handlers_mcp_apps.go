package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// MCP Apps（M8f-1，io.modelcontextprotocol/ui）宿主后端 REST API：
//   GET  /v1/mcp-apps/config                       沙箱源 + 总开关
//   GET  /v1/mcp-apps/resource?server_id=&uri=      UI 资源内容 + 安全配置
//   POST /v1/mcp-apps/views/{viewID}/rpc            View 发起的 JSON-RPC（前端宿主转发）
//   PUT  /v1/mcp-apps/views/{viewID}/state          widgetState 兼容持久化
//   PUT  /v1/mcp-apps/views/{viewID}/model-context   View 模型上下文更新兼容持久化
//
// 全部 view 相关请求先经 authorizeMCPAppView 校验：view 存在、属于请求方声明的
// session、服务器仍已连接；三者任一不满足分别映射 404/409/404。
// ============================================================================

// maxWidgetStateBytes ChatGPT widgetState 兼容持久化的大小上限（说明明确 64 KiB）。
const maxWidgetStateBytes = 64 * 1024

// rpcMethodToolCall / rpcMethodResourceRead / rpcMethodPing 是 View 可经本网关
// 调用的标准 MCP 协议方法子集（apps_spec.mdx §Standard MCP Messages）。
const (
	rpcMethodToolCall     = "tools/call"
	rpcMethodResourceRead = "resources/read"
	rpcMethodPing         = "ping"
)

// JSON-RPC 2.0 错误码：-32601/-32602 是标准码（方法不存在/参数非法）；-32000 是
// apps_spec.mdx 各请求示例统一使用的"Implementation-defined error"，用于工具
// 调用/资源读取执行失败（权限拒绝、服务器不存在等）——描述性文本放 Message，
// 不额外发明按 HTTP 状态码映射的错误码（JSON-RPC 与 HTTP 是两套独立的错误码
// 空间，不应混用）。
const (
	jsonRPCMethodNotFoundCode = -32601
	jsonRPCInvalidParamsCode  = -32602
	jsonRPCImplementationCode = -32000
)

type mcpAppsRPCRequest struct {
	JSONRPC   string          `json:"jsonrpc"`
	ID        json.RawMessage `json:"id,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"session_id"`
}

type mcpAppsRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpAppsRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *mcpAppsRPCError `json:"error,omitempty"`
}

// handleGetMCPAppsConfig GET /v1/mcp-apps/config。
func (s *Server) handleGetMCPAppsConfig(w http.ResponseWriter, r *http.Request) {
	// sandbox_origin 为空时前端用 location.protocol + hostname + sandbox_port 拼出沙箱源
	// （同一实例可能经 localhost / 局域网 IP / 域名访问，见 NewMCPAppsSandboxConfig）。
	httputil.WriteJSON(w, map[string]any{
		"sandbox_origin": s.appsSandboxCfg.SandboxOrigin,
		"sandbox_port":   s.appsSandboxBoundPort.Load(),
		"enabled":        s.appsSandboxCfg.Enabled,
	})
}

// handleGetMCPAppsResource GET /v1/mcp-apps/resource?server_id=&uri=。
func (s *Server) handleGetMCPAppsResource(w http.ResponseWriter, r *http.Request) {
	if !s.appsSandboxCfg.Enabled || s.mcpMgr == nil {
		http.Error(w, "mcp apps disabled", http.StatusNotImplemented)
		return
	}
	serverID := r.URL.Query().Get("server_id")
	uri := r.URL.Query().Get("uri")
	if serverID == "" || uri == "" {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "server_id and uri are required"), http.StatusBadRequest)
		return
	}
	res, err := s.mcpMgr.ReadUIResource(r.Context(), serverID, uri)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"html":           res.HTML,
		"mime_type":      res.MimeType,
		"csp":            res.CSP,
		"permissions":    res.Permissions,
		"domain":         res.Domain,
		"prefers_border": res.PrefersBorder,
	})
}

// authorizeMCPAppView 校验 view 存在、属于 sessionID、其所属服务器仍已连接
// （所有 view 相关请求的公共前置校验，见文件头注释）。
func (s *Server) authorizeMCPAppView(ctx context.Context, viewID, sessionID string) (*types.ChatAppViewRow, error) {
	if s.chatRepo == nil {
		return nil, apperr.New(apperr.CodeInternal, "mcp apps: chat repository not configured")
	}
	view, err := s.chatRepo.GetAppView(ctx, viewID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp apps: get view", err)
	}
	if view == nil {
		return nil, apperr.New(apperr.CodeNotFound, "mcp apps: view not found: "+viewID)
	}
	if sessionID == "" || view.SessionID != sessionID {
		return nil, apperr.New(apperr.CodeConflict, "mcp apps: view does not belong to the requesting session")
	}
	if s.mcpMgr == nil || !s.mcpMgr.IsServerConnected(view.ServerID) {
		return nil, apperr.New(apperr.CodeNotFound, "mcp apps: server not connected: "+view.ServerID)
	}
	return view, nil
}

// handleMCPAppsViewRPC POST /v1/mcp-apps/views/{viewID}/rpc。
func (s *Server) handleMCPAppsViewRPC(w http.ResponseWriter, r *http.Request) {
	viewID := r.PathValue("viewID")
	var req mcpAppsRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	view, err := s.authorizeMCPAppView(r.Context(), viewID, req.SessionID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, s.dispatchMCPAppsRPC(r.Context(), view, viewID, req))
}

// dispatchMCPAppsRPC 按方法分派：工具调用/资源读取经同一 CallToolAsApp/
// ReadResourceAsApp 入口（HE-3）；ping 原地应答；其余方法返回 JSON-RPC
// -32601（apps_spec.mdx §Standard MCP Messages 之外的方法本网关不代理）。
func (s *Server) dispatchMCPAppsRPC(ctx context.Context, view *types.ChatAppViewRow, viewID string, req mcpAppsRPCRequest) mcpAppsRPCResponse {
	resp := mcpAppsRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case rpcMethodPing:
		resp.Result = json.RawMessage(`{}`)
	case rpcMethodToolCall:
		resp.Result, resp.Error = s.dispatchMCPAppsToolCall(ctx, view, viewID, req.Params)
	case rpcMethodResourceRead:
		resp.Result, resp.Error = s.dispatchMCPAppsResourceRead(ctx, view, req.Params)
	default:
		resp.Error = &mcpAppsRPCError{Code: jsonRPCMethodNotFoundCode, Message: "method not found: " + req.Method}
	}
	return resp
}

// dispatchMCPAppsToolCall 工具调用方法：只允许 view 所属服务器上 visibility 含
// "app" 的工具，经 ExecuteTool 执行（HE-3），结果不进模型上下文，原样透传给 View。
func (s *Server) dispatchMCPAppsToolCall(ctx context.Context, view *types.ChatAppViewRow, viewID string, params json.RawMessage) (json.RawMessage, *mcpAppsRPCError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &mcpAppsRPCError{Code: jsonRPCInvalidParamsCode, Message: "invalid tool call params"}
	}
	raw, err := s.mcpMgr.CallToolAsApp(ctx, view.ServerID, p.Name, p.Arguments, view.SessionID, viewID)
	if err != nil {
		slog.Warn("mcp apps: view rpc tool call failed", "view_id", viewID, "server", view.ServerID, "tool", p.Name, "err", err)
		return nil, &mcpAppsRPCError{Code: jsonRPCImplementationCode, Message: err.Error()}
	}
	return raw, nil
}

// dispatchMCPAppsResourceRead 资源读取方法：只允许该服务器的资源（serverID 取自
// view 记录，天然限定命名空间，见 ReadResourceAsApp 注释）。
func (s *Server) dispatchMCPAppsResourceRead(ctx context.Context, view *types.ChatAppViewRow, params json.RawMessage) (json.RawMessage, *mcpAppsRPCError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
		return nil, &mcpAppsRPCError{Code: jsonRPCInvalidParamsCode, Message: "invalid resource read params"}
	}
	contents, err := s.mcpMgr.ReadResourceAsApp(ctx, view.ServerID, p.URI)
	if err != nil {
		return nil, &mcpAppsRPCError{Code: jsonRPCImplementationCode, Message: err.Error()}
	}
	result, marshalErr := json.Marshal(map[string]any{"contents": contents})
	if marshalErr != nil {
		return nil, &mcpAppsRPCError{Code: jsonRPCImplementationCode, Message: "encode result failed"}
	}
	return result, nil
}

// handleMCPAppsViewState PUT /v1/mcp-apps/views/{viewID}/state
// body: {"session_id":"...", "widget_state": <any JSON>}（ChatGPT widgetState 兼容）。
func (s *Server) handleMCPAppsViewState(w http.ResponseWriter, r *http.Request) {
	viewID := r.PathValue("viewID")
	var req struct {
		SessionID   string          `json:"session_id"`
		WidgetState json.RawMessage `json:"widget_state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if len(req.WidgetState) > maxWidgetStateBytes {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "widget_state exceeds 64 KiB limit"), http.StatusBadRequest)
		return
	}
	view, err := s.authorizeMCPAppView(r.Context(), viewID, req.SessionID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	state := string(req.WidgetState)
	if state == "" {
		state = "{}"
	}
	if err := s.chatRepo.UpdateAppViewWidgetState(r.Context(), view.ViewID, state); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	httputil.WriteJSON(w, map[string]any{"status": "ok"})
}

// handleMCPAppsViewModelContext PUT /v1/mcp-apps/views/{viewID}/model-context
// body: {"session_id":"...", "content": [...], "structuredContent": {...}}
// （apps_spec.mdx §MCP Apps Specific Messages，View 更新模型上下文的请求）。
// 按 (session, server) 持久化最新一份，下一轮提示词装配时注入为外部不可信内容，
// 注入后清空（见 internal/gateway/session 的会话编排消费端；本 handler 只负责
// 落库，不做注入）。
func (s *Server) handleMCPAppsViewModelContext(w http.ResponseWriter, r *http.Request) {
	viewID := r.PathValue("viewID")
	var req struct {
		SessionID         string          `json:"session_id"`
		Content           json.RawMessage `json:"content,omitempty"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	view, err := s.authorizeMCPAppView(r.Context(), viewID, req.SessionID)
	if err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	payload, marshalErr := json.Marshal(map[string]any{
		"content":           rawOrNil(req.Content),
		"structuredContent": rawOrNil(req.StructuredContent),
	})
	if marshalErr != nil {
		httputil.RespondError(w, "", apperr.Wrap(apperr.CodeInternal, "mcp apps: encode model context", marshalErr), http.StatusInternalServerError)
		return
	}
	if err := s.chatRepo.UpsertSessionModelContextServer(r.Context(), view.SessionID, view.ServerID, string(payload)); err != nil {
		httputil.RespondError(w, "", err, apperr.HTTPStatus(apperr.CodeOf(err)))
		return
	}
	slog.Info("mcp apps: model context updated", "session", view.SessionID, "server", view.ServerID, "view_id", viewID)
	httputil.WriteJSON(w, map[string]any{"status": "ok"})
}

func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
