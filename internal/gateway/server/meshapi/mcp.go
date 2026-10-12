package meshapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/version"
)

// MCP 服务端：2026-07-28 无状态核心（server/discover + 每请求 _meta）为主，兼容 2025-11-25
// 旧纪元 initialize（Claude 等尚未升级的客户端）。只暴露委派类工具，不暴露本机工具目录
// （ADR-0111 决策三第 2 条）。
const (
	mcpModernVersion = "2026-07-28"
	mcpLegacyVersion = "2025-11-25"
	mcpTasksExt      = "io.modelcontextprotocol/tasks"
	metaProtoVersion = "io.modelcontextprotocol/protocolVersion"
	metaClientCaps   = "io.modelcontextprotocol/clientCapabilities"

	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
	rpcUnsupportedVer = -32022 // 2026-07-28 basic/versioning UnsupportedProtocolVersion

	// mcpTaskPollMs 建议对端的轮询间隔；mcpMaxBody 单个请求体上限（委派输入另有 MaxInputBytes）。
	mcpTaskPollMs = int64(2000)
	mcpMaxBody    = 1 << 20

	toolDelegate   = "a2a_delegate"
	toolListAgents = "a2a_list_agents"
	toolGetTask    = "get_task"
	toolCancelTask = "cancel_task"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpServer 委派类工具的 MCP 服务端。
type mcpServer struct {
	svc       *mesh.Service
	agentName string
}

func (m *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// 无状态纪元没有 GET 订阅流；旧纪元的 GET SSE 通知流本服务也不提供（无服务端主动通知）。
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody+1))
	if err != nil || len(body) > mcpMaxBody {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: rpcInvalidRequest, Message: "request body too large or unreadable"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: rpcParseError, Message: "invalid JSON-RPC request"}})
		return
	}
	if len(req.ID) == 0 { // 通知：无需应答
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := m.dispatch(r.Context(), &req)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	if rerr == nil && result == nil {
		resp.Result = struct{}{}
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("meshapi: write MCP response failed", "err", err)
	}
}

// reqMeta 新纪元每请求 _meta 中与本服务相关的部分。
type reqMeta struct {
	version  string
	tasksExt bool
}

func parseMeta(params json.RawMessage) reqMeta {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	var m reqMeta
	// _meta 缺失或不是对象 = 旧纪元 / 未声明扩展的客户端，按缺省处理，不算错误。
	if json.Unmarshal(params, &p) != nil {
		return m
	}
	if raw, ok := p.Meta[metaProtoVersion]; ok && json.Unmarshal(raw, &m.version) != nil {
		m.version = "invalid"
	}
	var caps struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(p.Meta[metaClientCaps], &caps) == nil {
		_, m.tasksExt = caps.Extensions[mcpTasksExt]
	}
	return m
}

func (m *mcpServer) dispatch(ctx context.Context, req *rpcRequest) (any, *rpcError) {
	meta := parseMeta(req.Params)
	if meta.version != "" && meta.version != mcpModernVersion {
		return nil, &rpcError{Code: rpcUnsupportedVer, Message: "unsupported protocol version",
			Data: map[string]any{"supported": []string{mcpModernVersion}, "requested": meta.version}}
	}
	switch req.Method {
	case "server/discover":
		return m.discoverResult(), nil
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpLegacyVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "polaris-mesh", "version": version.Version},
			"instructions":    m.instructions(),
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		return m.callTool(ctx, req.Params, meta)
	case "tasks/get":
		return m.tasksGet(ctx, req.Params)
	case "tasks/cancel":
		return m.tasksCancel(ctx, req.Params)
	case "tasks/update":
		// 本服务的任务从不进入 input_required（审批只在本节点本地完成，决策五），
		// 因此没有任何合法的 inputResponses 可收。明确拒绝，而不是静默接受后丢弃。
		return nil, &rpcError{Code: rpcInvalidParams,
			Message: "this server never requests input; approvals are resolved by a local human only"}
	default:
		return nil, &rpcError{Code: rpcMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (m *mcpServer) instructions() string {
	return fmt.Sprintf("Polaris node %s (%s). Use a2a_delegate to hand a task to this node. "+
		"Tasks run read-only by default; actions needing approval wait for a human on this node.",
		m.agentName, m.svc.Self().NodeID)
}

func (m *mcpServer) discoverResult() map[string]any {
	return map[string]any{
		"supportedVersions": []string{mcpModernVersion},
		"capabilities": map[string]any{
			"tools":      map[string]any{"listChanged": false},
			"extensions": map[string]any{mcpTasksExt: map[string]any{}},
		},
		"serverInfo":   map[string]any{"name": "polaris-mesh", "version": version.Version},
		"instructions": m.instructions(),
	}
}

func toolDefs() []map[string]any {
	str := map[string]any{"type": "string"}
	return []map[string]any{
		{
			"name": toolDelegate,
			"description": "Delegate a task to this Polaris node. Long-running: returns an MCP task when the client " +
				"supports the tasks extension, otherwise a task id to poll with get_task.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_agent":    str,
					"context_summary": map[string]any{"type": "string", "description": "the task to perform"},
					"namespace":       str,
					"spawn_depth":     map[string]any{"type": "integer", "minimum": 1},
					"chain":           map[string]any{"type": "array", "items": str},
				},
				"required": []string{"context_summary"},
			},
		},
		{
			"name":        toolListAgents,
			"description": "List the agents addressable on this node.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        toolGetTask,
			"description": "Get the state and result of a task previously delegated to this node.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"task_id": str},
				"required": []string{"task_id"}},
		},
		{
			"name":        toolCancelTask,
			"description": "Cancel a task previously delegated to this node.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"task_id": str},
				"required": []string{"task_id"}},
		},
	}
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type delegateArgs struct {
	TargetAgent    string   `json:"target_agent"`
	ContextSummary string   `json:"context_summary"`
	Namespace      string   `json:"namespace"`
	SpawnDepth     *int     `json:"spawn_depth"`
	Chain          []string `json:"chain"`
}

func textResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

func (m *mcpServer) callTool(ctx context.Context, raw json.RawMessage, meta reqMeta) (any, *rpcError) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, &rpcError{Code: rpcInvalidRequest, Message: "unauthenticated"}
	}
	var call toolCallParams
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid tools/call params"}
	}
	switch call.Name {
	case toolListAgents:
		// ADR-0084 约定的 a2a_list_agents 响应形状：[{name, description}]（文本内容即 JSON）。
		list, _ := json.Marshal([]map[string]string{{"name": "default",
			"description": fmt.Sprintf("Polaris node %s (%s)", m.agentName, m.svc.Self().NodeID)}})
		return textResult(string(list), false), nil
	case toolDelegate:
		return m.delegate(ctx, p, call.Arguments, meta)
	case toolGetTask, toolCancelTask:
		var a struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.TaskID == "" {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "task_id is required"}
		}
		var t *mesh.Task
		var err error
		if call.Name == toolGetTask {
			t, err = m.svc.Get(ctx, p, a.TaskID)
		} else {
			t, err = m.svc.Cancel(ctx, p, a.TaskID)
		}
		if err != nil {
			return textResult(err.Error(), true), nil
		}
		return textResult(describeTask(t), false), nil
	default:
		return nil, &rpcError{Code: rpcInvalidParams, Message: "unknown tool: " + call.Name}
	}
}

func (m *mcpServer) delegate(ctx context.Context, p mesh.Principal, raw json.RawMessage, meta reqMeta) (any, *rpcError) {
	var a delegateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid a2a_delegate arguments"}
	}
	var deleg *mesh.Delegation
	if a.SpawnDepth != nil || len(a.Chain) > 0 {
		deleg = &mesh.Delegation{Chain: a.Chain}
		if a.SpawnDepth != nil {
			deleg.SpawnDepth = *a.SpawnDepth
		}
	}
	t, err := m.svc.Submit(ctx, p, mesh.SubmitRequest{Protocol: mesh.ProtocolMCP, TargetAgent: a.TargetAgent,
		Text: a.ContextSummary, Delegation: deleg})
	if err != nil {
		if apperr.CodeOf(err) == apperr.CodeInvalidInput {
			return nil, &rpcError{Code: rpcInvalidParams, Message: err.Error()}
		}
		return textResult(err.Error(), true), nil
	}
	if t.State == mesh.StateRejected {
		return textResult("delegation rejected by node "+m.svc.Self().NodeID+": "+t.StatusMessage, true), nil
	}
	if meta.tasksExt {
		state := m.taskState(t)
		state["resultType"] = "task"
		return state, nil
	}
	// 不支持 tasks 扩展的客户端（旧纪元 / 未声明）：不阻塞 HTTP 连接，返回任务 ID 供 get_task 轮询。
	return textResult(fmt.Sprintf("task %s accepted by node %s; poll with get_task", t.ID, m.svc.Self().NodeID), false), nil
}

// taskState 委派任务 → MCP Tasks 的 Task 对象（tasks/get 响应与 CreateTaskResult 共用）。
// 映射：submitted/working/input_required → working（input_required 不外露为 MCP 的
// input_required，见 tasks/update 注释）；completed → completed + result；failed/rejected → failed + error；
// canceled → cancelled。
func (m *mcpServer) taskState(t *mesh.Task) map[string]any {
	poll := mcpTaskPollMs
	ttl := t.ExpiresAt.Sub(t.CreatedAt).Milliseconds()
	out := map[string]any{
		"taskId":         t.ID,
		"createdAt":      t.CreatedAt.UTC().Format(time.RFC3339),
		"lastUpdatedAt":  t.UpdatedAt.UTC().Format(time.RFC3339),
		"ttlMs":          ttl,
		"pollIntervalMs": poll,
	}
	switch t.State {
	case mesh.StateCompleted:
		out["status"] = "completed"
		out["result"] = textResult(t.Result, false)
	case mesh.StateFailed, mesh.StateRejected:
		out["status"] = "failed"
		out["error"] = rpcError{Code: rpcInternalError, Message: firstNonEmpty(t.StatusMessage, "task failed")}
	case mesh.StateCanceled:
		out["status"] = "cancelled"
	default:
		out["status"] = "working"
	}
	if t.StatusMessage != "" {
		out["statusMessage"] = t.StatusMessage
	}
	return out
}

func (m *mcpServer) taskParam(ctx context.Context, raw json.RawMessage) (mesh.Principal, string, *rpcError) {
	p, ok := principalFrom(ctx)
	if !ok {
		return p, "", &rpcError{Code: rpcInvalidRequest, Message: "unauthenticated"}
	}
	var a struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.TaskID == "" {
		return p, "", &rpcError{Code: rpcInvalidParams, Message: "taskId is required"}
	}
	return p, a.TaskID, nil
}

func (m *mcpServer) tasksGet(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	p, id, rerr := m.taskParam(ctx, raw)
	if rerr != nil {
		return nil, rerr
	}
	t, err := m.svc.Get(ctx, p, id)
	if err != nil {
		return nil, appErrToRPC(err)
	}
	return m.taskState(t), nil
}

func (m *mcpServer) tasksCancel(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	p, id, rerr := m.taskParam(ctx, raw)
	if rerr != nil {
		return nil, rerr
	}
	t, err := m.svc.Cancel(ctx, p, id)
	if err != nil && apperr.CodeOf(err) != apperr.CodeConflict {
		return nil, appErrToRPC(err)
	}
	return m.taskState(t), nil
}

func appErrToRPC(err error) *rpcError {
	switch apperr.CodeOf(err) {
	case apperr.CodeNotFound, apperr.CodeInvalidInput:
		return &rpcError{Code: rpcInvalidParams, Message: err.Error()}
	case apperr.CodeForbidden:
		return &rpcError{Code: rpcInvalidRequest, Message: err.Error()}
	default:
		return &rpcError{Code: rpcInternalError, Message: "internal error"}
	}
}

func describeTask(t *mesh.Task) string {
	b, _ := json.Marshal(map[string]any{"task_id": t.ID, "state": t.State, "status_message": t.StatusMessage,
		"result": t.Result, "updated_at": t.UpdatedAt.UTC().Format(time.RFC3339Nano)})
	return string(b)
}
