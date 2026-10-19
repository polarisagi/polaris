package codeact

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/config"
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
	return func(c *CodeAct) { c.toolExec = exec }
}

// WithPolicy 注入安全策略配置。
func WithPolicy(pol policy.Policy) CodeActOption {
	return func(c *CodeAct) { c.policy = pol }
}

// BridgeServer 提供 CodeAct 解释器与内核间的专用 Unix Socket 传输桥（ADR-0115 WP5，T6 原则）。
type BridgeServer struct {
	sockPath       string
	token          string
	listener       net.Listener
	toolExec       ToolBridgeExecutor
	pol            policy.Policy
	profileChecker ProfileChecker
	currentTaint   types.TaintLevel
	maxTaint       types.TaintLevel
	mu             sync.Mutex
	closed         chan struct{}
}

type bridgeRequest struct {
	Token  string            `json:"token,omitempty"`
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

func createBridgeListener(explicitDataDir string) (net.Listener, string, error) {
	dataDir := explicitDataDir
	if dataDir == "" {
		if d, err := config.ResolveDataDir(""); err == nil && d != "" {
			dataDir = d
		} else {
			dataDir = os.TempDir()
		}
	}
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInternal, "code_act bridge: mkdir run dir failed", err)
	}
	_ = os.Chmod(runDir, 0o700)

	sockPath := filepath.Join(runDir, fmt.Sprintf("pb_%s.sock", uuid.NewString()[:8]))
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		slog.Debug("code_act bridge: cleanup old socket", "path", sockPath, "err", err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInternal, "code_act bridge: listen unix failed", err)
	}

	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = l.Close()
		_ = os.Remove(sockPath)
		return nil, "", apperr.Wrap(apperr.CodeInternal, "code_act bridge: chmod failed", err)
	}
	return l, sockPath, nil
}

// StartBridgeServer 启动工具桥本地 Unix Socket 服务（R2-15：迁入 DataDir/run/ 0700 目录与 Token 鉴权）。
func StartBridgeServer(ctx context.Context, toolExec ToolBridgeExecutor, pol policy.Policy, initialTaint types.TaintLevel, opts ...BridgeOption) (*BridgeServer, error) {
	if toolExec == nil {
		return nil, apperr.New(apperr.CodeInvalidInput, "code_act bridge: tool executor is nil")
	}
	if pol.Profile == "" {
		pol = policy.ProfilePolicy(policy.ProfileStandard)
	}

	cfg := parseBridgeConfig(opts)
	if cfg.token == "" {
		cfg.token = uuid.NewString()
	}

	l, sockPath, err := createBridgeListener(cfg.dataDir)
	if err != nil {
		return nil, err
	}

	srv := &BridgeServer{
		sockPath:       sockPath,
		token:          cfg.token,
		listener:       l,
		toolExec:       toolExec,
		pol:            pol,
		profileChecker: cfg.profileChecker,
		currentTaint:   initialTaint,
		maxTaint:       initialTaint,
		closed:         make(chan struct{}),
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

// Token 返回本会话生成的鉴权令牌（R2-15）。
func (s *BridgeServer) Token() string {
	return s.token
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
			return
		}
		// 鉴权校验（R2-15）：Token 不匹配直接断开连接
		if s.token != "" && req.Token != s.token {
			s.writeResponse(conn, bridgeResponse{OK: false, Error: "unauthorized: invalid bridge token"})
			return
		}
		resp := s.dispatch(ctx, req)
		s.writeResponse(conn, resp)
	}
}

func (s *BridgeServer) writeResponse(conn net.Conn, resp bridgeResponse) {
	bytes, err := json.Marshal(resp)
	if err != nil {
		slog.Warn("code_act bridge: marshal response failed", "err", err)
		return
	}
	bytes = append(bytes, '\n')
	if _, err := conn.Write(bytes); err != nil {
		slog.Debug("code_act bridge: write response failed", "err", err)
	}
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

	// 档位规则判定（R2-15 复用 policy.WritesState 对 MCP/Skill fail-closed）
	if !s.pol.CodeActAllToolsAllowed {
		if policy.WritesState(toolName, t, true) {
			return bridgeResponse{
				OK:    false,
				Error: fmt.Sprintf("tool %q writes state (capability %d, source %s), blocked in %s profile", toolName, t.Capability, t.Source, s.pol.Profile),
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

	// 角色权限判定：桥接执行前调用注入的 ProfileChecker（R2-15）
	if s.profileChecker != nil {
		if err := s.profileChecker.CheckProfileTool(toolName, ""); err != nil {
			return bridgeResponse{
				OK:    false,
				Error: fmt.Sprintf("profile check failed: %v", err),
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
			if policy.WritesState(t.Name, t, true) {
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
		if s.profileChecker != nil {
			if err := s.profileChecker.CheckProfileTool(t.Name, ""); err != nil {
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
		if err := os.Remove(s.sockPath); err != nil && !os.IsNotExist(err) {
			slog.Debug("code_act bridge: remove socket on close failed", "path", s.sockPath, "err", err)
		}
	}
}
