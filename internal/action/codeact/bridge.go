package codeact

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ToolBridgeExecutor CodeAct 工具桥调用的消费端接口（符合 internal/action/CLAUDE.md 消费端定义规范）。
type ToolBridgeExecutor interface {
	ExecuteTool(ctx context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error)
	List() []types.Tool
	Lookup(name string) (types.Tool, error)
}

// WithToolExecutor 注入工具桥执行器。
func WithToolExecutor(exec ToolBridgeExecutor) CodeActOption {
	return func(c *CodeAct) {
		c.toolExec = exec
	}
}

// WithPolicy 注入安全策略配置。
func WithPolicy(pol policy.Policy) CodeActOption {
	return func(c *CodeAct) {
		c.policy = pol
	}
}

// BridgeServer 提供 CodeAct 解释器与内核间的专用 Unix Socket 传输桥（ADR-0115 WP5，T6 原则）。
type BridgeServer struct {
	sockPath     string
	listener     net.Listener
	toolExec     ToolBridgeExecutor
	pol          policy.Policy
	currentTaint types.TaintLevel
	maxTaint     types.TaintLevel
	mu           sync.Mutex
	closed       chan struct{}
}

type bridgeRequest struct {
	Action string            `json:"action"` // "call" | "call_many" | "tools"
	Tool   string            `json:"tool,omitempty"`
	Args   map[string]any    `json:"args,omitempty"`
	Calls  []bridgeCallParam `json:"calls,omitempty"`
}

type bridgeCallParam struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type bridgeResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Result  any    `json:"result,omitempty"`
	Results []any  `json:"results,omitempty"`
	Tools   any    `json:"tools,omitempty"`
}

// StartBridgeServer 启动工具桥本地 Unix Socket 服务。
func StartBridgeServer(ctx context.Context, toolExec ToolBridgeExecutor, pol policy.Policy, initialTaint types.TaintLevel) (*BridgeServer, error) {
	if toolExec == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "code_act bridge: tool executor is nil")
	}

	// Unix domain socket 路径长度限制（macOS 104 字节，Linux 108 字节），使用简短前缀
	sockPath := filepath.Join("/tmp", fmt.Sprintf("pb_%s.sock", uuid.NewString()[:8]))
	_ = os.Remove(sockPath) // 防御性清理旧残留
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act bridge: listen unix failed", err)
	}

	if err := os.Chmod(sockPath, 0600); err != nil {
		_ = l.Close()
		_ = os.Remove(sockPath)
		return nil, apperr.Wrap(apperr.CodeInternal, "code_act bridge: chmod failed", err)
	}

	srv := &BridgeServer{
		sockPath:     sockPath,
		listener:     l,
		toolExec:     toolExec,
		pol:          pol,
		currentTaint: initialTaint,
		maxTaint:     initialTaint,
		closed:       make(chan struct{}),
	}

	concurrent.SafeGo(ctx, "code_act_bridge_accept", func(subCtx context.Context) {
		for {
			conn, acceptErr := l.Accept()
			if acceptErr != nil {
				select {
				case <-srv.closed:
					return
				case <-subCtx.Done():
					return
				default:
					return
				}
			}
			concurrent.SafeGo(subCtx, "code_act_bridge_conn", func(connCtx context.Context) {
				srv.handleConn(connCtx, conn)
			})
		}
	})

	return srv, nil
}

// SocketPath 返回监听的 Unix Socket 路径。
func (s *BridgeServer) SocketPath() string {
	return s.sockPath
}

// HighestTaint 获取会话期间子调用达到的最高污点等级（Taint 不得静默丢失，HE-2）。
func (s *BridgeServer) HighestTaint() types.TaintLevel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxTaint
}

func (s *BridgeServer) recordTaint(t types.TaintLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t > s.maxTaint {
		s.maxTaint = t
	}
}

func (s *BridgeServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req bridgeRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeResponse(conn, bridgeResponse{OK: false, Error: "invalid request payload: " + err.Error()})
			continue
		}
		resp := s.dispatch(ctx, req)
		s.writeResponse(conn, resp)
	}
}

func (s *BridgeServer) writeResponse(conn net.Conn, resp bridgeResponse) {
	bytes, err := json.Marshal(resp)
	if err != nil {
		return
	}
	bytes = append(bytes, '\n')
	_, _ = conn.Write(bytes)
}

func (s *BridgeServer) dispatch(ctx context.Context, req bridgeRequest) bridgeResponse {
	switch req.Action {
	case "call":
		return s.handleCall(ctx, req.Tool, req.Args)
	case "call_many":
		return s.handleCallMany(ctx, req.Calls)
	case "tools":
		return s.handleTools()
	default:
		return bridgeResponse{OK: false, Error: fmt.Sprintf("unsupported action %q", req.Action)}
	}
}

func (s *BridgeServer) handleCall(ctx context.Context, toolName string, args map[string]any) bridgeResponse {
	if toolName == "" {
		return bridgeResponse{OK: false, Error: "tool name is required"}
	}
	t, err := s.toolExec.Lookup(toolName)
	if err != nil {
		return bridgeResponse{OK: false, Error: fmt.Sprintf("tool %q not found: %v", toolName, err)}
	}

	// 档位规则判定（经 ProfilePolicy 纯字段）：standard / hardened 只允许 CapReadOnly 且幂等的工具；full 允许全部
	if !s.pol.CodeActAllToolsAllowed {
		if t.Capability != types.CapReadOnly {
			return bridgeResponse{
				OK:    false,
				Error: fmt.Sprintf("tool %q requires capability %d (not CapReadOnly), blocked in %s profile", toolName, t.Capability, s.pol.Profile),
			}
		}
		for _, se := range t.SideEffects {
			if se != types.SideNone && se != "" {
				return bridgeResponse{
					OK:    false,
					Error: fmt.Sprintf("tool %q has side effect %q (not idempotent), blocked in %s profile", toolName, se, s.pol.Profile),
				}
			}
		}
	}

	inputBytes, err := json.Marshal(args)
	if err != nil {
		return bridgeResponse{OK: false, Error: "marshal args failed: " + err.Error()}
	}

	res, execErr := s.toolExec.ExecuteTool(ctx, toolName, inputBytes, s.currentTaint)
	if execErr != nil {
		return bridgeResponse{OK: false, Error: execErr.Error()}
	}
	if !res.Success {
		return bridgeResponse{OK: false, Error: res.Error}
	}

	s.recordTaint(res.TaintLevel)

	var parsed any
	if err := json.Unmarshal(res.Output, &parsed); err != nil {
		parsed = string(res.Output)
	}
	return bridgeResponse{OK: true, Result: parsed}
}

func (s *BridgeServer) handleCallMany(ctx context.Context, calls []bridgeCallParam) bridgeResponse {
	if len(calls) == 0 {
		return bridgeResponse{OK: true, Results: []any{}}
	}
	results := make([]any, len(calls))
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, c := range calls {
		idx := i
		item := c
		concurrent.SafeGo(ctx, "code_act_bridge_call_many", func(subCtx context.Context) {
			defer wg.Done()
			subResp := s.handleCall(subCtx, item.Tool, item.Args)
			if !subResp.OK {
				results[idx] = map[string]any{"ok": false, "error": subResp.Error}
			} else {
				results[idx] = map[string]any{"ok": true, "result": subResp.Result}
			}
		})
	}
	wg.Wait()
	return bridgeResponse{OK: true, Results: results}
}

func (s *BridgeServer) handleTools() bridgeResponse {
	tools := s.toolExec.List()
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if !s.pol.CodeActAllToolsAllowed {
			if t.Capability != types.CapReadOnly {
				continue
			}
			nonIdempotent := false
			for _, se := range t.SideEffects {
				if se != types.SideNone && se != "" {
					nonIdempotent = true
					break
				}
			}
			if nonIdempotent {
				continue
			}
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"readonly":    t.Capability == types.CapReadOnly,
		})
	}
	return bridgeResponse{OK: true, Tools: out}
}

// Close 关闭服务并清理 Socket 文件。
func (s *BridgeServer) Close() {
	select {
	case <-s.closed:
		return
	default:
		close(s.closed)
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.sockPath != "" {
		_ = os.Remove(s.sockPath)
	}
}

// PythonPolarisModule 是注入到 Python 解释器环境的 polaris 客户端源码。
const PythonPolarisModule = `import os, sys, socket, json

class _PolarisClient:
    def __init__(self):
        self._sock_path = os.environ.get("POLARIS_BRIDGE_SOCKET", "")

    def _send(self, req):
        if not self._sock_path:
            raise RuntimeError("POLARIS_BRIDGE_SOCKET environment variable not set")
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._sock_path)
        try:
            payload = json.dumps(req) + "\n"
            s.sendall(payload.encode("utf-8"))
            buf = b""
            while b"\n" not in buf:
                chunk = s.recv(4096)
                if not chunk:
                    break
                buf += chunk
            line = buf.split(b"\n")[0].decode("utf-8")
            return json.loads(line)
        finally:
            s.close()

    def call(self, tool_name, **kwargs):
        resp = self._send({"action": "call", "tool": tool_name, "args": kwargs})
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", f"tool {tool_name} failed"))
        return resp.get("result")

    def call_many(self, calls):
        normalized = []
        for c in calls:
            if isinstance(c, dict):
                normalized.append({"tool": c.get("tool", ""), "args": c.get("args", {})})
            elif isinstance(c, (list, tuple)) and len(c) == 2:
                normalized.append({"tool": c[0], "args": c[1]})
        resp = self._send({"action": "call_many", "calls": normalized})
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", "call_many failed"))
        return resp.get("results")

    def tools(self):
        resp = self._send({"action": "tools"})
        if not resp.get("ok"):
            raise RuntimeError(resp.get("error", "list tools failed"))
        return resp.get("tools")

_client = _PolarisClient()
call = _client.call
call_many = _client.call_many
tools = _client.tools
`

// EnsurePolarisPythonModule 确保指定目录下存在 polaris.py 模块。
func EnsurePolarisPythonModule(dir string) error {
	path := filepath.Join(dir, "polaris.py")
	if err := os.WriteFile(path, []byte(PythonPolarisModule), 0o600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ensure polaris module", err)
	}
	return nil
}
