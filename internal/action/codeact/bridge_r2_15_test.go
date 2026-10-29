package codeact_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockMCPToolRegistry struct {
	tools map[string]types.Tool
}

func (m *mockMCPToolRegistry) Lookup(name string) (types.Tool, error) {
	t, ok := m.tools[name]
	if !ok {
		return types.Tool{}, apperr.New(apperr.CodeNotFound, "tool not found")
	}
	return t, nil
}

func (m *mockMCPToolRegistry) List() []types.Tool {
	out := make([]types.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t)
	}
	return out
}

func (m *mockMCPToolRegistry) ExecuteTool(_ context.Context, name string, _ []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	return &types.ToolResult{
		Success:    true,
		Output:     []byte(`{"result":"ok"}`),
		TaintLevel: taintLevel,
	}, nil
}

// TestR2_15_MCPZeroCapability_BlockedInStandardProfile 验证 MCP 工具（即使 Capability 为零值/CapReadOnly）
// 在 standard 档下也必须 fail-closed 拦截，不得被 code_act 调用。
func TestR2_15_MCPZeroCapability_BlockedInStandardProfile(t *testing.T) {
	reg := &mockMCPToolRegistry{
		tools: map[string]types.Tool{
			"mcp__fs__write_file": {
				Name:        "mcp__fs__write_file",
				Source:      types.ToolMCP,
				Capability:  types.CapReadOnly, // 零值能力
				SideEffects: nil,
			},
		},
	}

	srv, err := codeact.StartBridgeServer(context.Background(), reg, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatalf("StartBridgeServer failed: %v", err)
	}
	defer srv.Close()

	// 1. 调用该 MCP 工具应当被拦截
	resp, err := sendBridgeRequestWithToken(srv.SocketPath(), srv.Token(), map[string]any{
		"action": "call",
		"tool":   "mcp__fs__write_file",
		"args":   map[string]any{"path": "/etc/passwd"},
	})
	if err != nil {
		t.Fatalf("sendBridgeRequest failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("expected mcp tool with zero capability to be blocked in standard profile, but got ok=true")
	}

	// 2. tools 列表中也不得列出该工具
	toolsResp, err := sendBridgeRequestWithToken(srv.SocketPath(), srv.Token(), map[string]any{
		"action": "tools",
	})
	if err != nil {
		t.Fatalf("tools request failed: %v", err)
	}
	toolsList, _ := toolsResp["tools"].([]any)
	for _, item := range toolsList {
		tm, _ := item.(map[string]any)
		if tm["name"] == "mcp__fs__write_file" {
			t.Fatalf("expected mcp__fs__write_file to be omitted from tools list in standard profile")
		}
	}
}

type mockProfileChecker struct {
	deniedTools map[string]string
}

func (m *mockProfileChecker) CheckProfileTool(toolName, delegateTarget string) error {
	if reason, denied := m.deniedTools[toolName]; denied {
		return errors.New("denied: " + reason)
	}
	return nil
}

// TestR2_15_ProfileChecker_SubagentRestrictionsEnforced 验证注入的 ProfileChecker 在 code_act 工具桥中生效。
func TestR2_15_ProfileChecker_SubagentRestrictionsEnforced(t *testing.T) {
	reg := &mockMCPToolRegistry{
		tools: map[string]types.Tool{
			"read_file": {
				Name:        "read_file",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			"restricted_tool": {
				Name:        "restricted_tool",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
		},
	}

	checker := &mockProfileChecker{
		deniedTools: map[string]string{
			"restricted_tool": "tool not in subagent whitelist",
		},
	}

	srv, err := codeact.StartBridgeServer(
		context.Background(),
		reg,
		policy.ProfilePolicy(policy.ProfileStandard),
		types.TaintLow,
		codeact.WithBridgeProfileChecker(checker),
	)
	if err != nil {
		t.Fatalf("StartBridgeServer failed: %v", err)
	}
	defer srv.Close()

	// restricted_tool 应当被 checkProfileTool 拦截
	resp, err := sendBridgeRequestWithToken(srv.SocketPath(), srv.Token(), map[string]any{
		"action": "call",
		"tool":   "restricted_tool",
		"args":   map[string]any{},
	})
	if err != nil {
		t.Fatalf("sendBridgeRequest failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("expected restricted_tool to be denied by profile checker, got ok=true")
	}

	// read_file 应当放行
	respAllowed, err := sendBridgeRequestWithToken(srv.SocketPath(), srv.Token(), map[string]any{
		"action": "call",
		"tool":   "read_file",
		"args":   map[string]any{},
	})
	if err != nil {
		t.Fatalf("sendBridgeRequest for read_file failed: %v", err)
	}
	if ok, _ := respAllowed["ok"].(bool); !ok {
		t.Fatalf("expected read_file to be allowed, got error: %v", respAllowed["error"])
	}
}

// TestR2_15_SocketLocationAndTokenAuth 验证 Socket 位于 DataDir/run/（0700权限）且强制 Token 鉴权。
func TestR2_15_SocketLocationAndTokenAuth(t *testing.T) {
	tmpDataDir, err := os.MkdirTemp("/tmp", "pdata")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDataDir)
	reg := &mockMCPToolRegistry{
		tools: map[string]types.Tool{
			"read_file": {
				Name:        "read_file",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
		},
	}

	srv, err := codeact.StartBridgeServer(
		context.Background(),
		reg,
		policy.ProfilePolicy(policy.ProfileStandard),
		types.TaintLow,
		codeact.WithBridgeDataDir(tmpDataDir),
	)
	if err != nil {
		t.Fatalf("StartBridgeServer failed: %v", err)
	}
	defer srv.Close()

	// 1. 验证 socket 位于 tmpDataDir/run/ 下
	runDir := filepath.Join(tmpDataDir, "run")
	if !strings.HasPrefix(srv.SocketPath(), runDir) {
		t.Fatalf("expected socket path inside %s, got %s", runDir, srv.SocketPath())
	}
	dirInfo, err := os.Stat(runDir)
	if err != nil {
		t.Fatalf("stat runDir failed: %v", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("expected runDir perm 0700, got %o", dirInfo.Mode().Perm())
	}

	// 2. 携带错误 token 应该被拒绝且连接断开
	conn, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	badPayload, _ := json.Marshal(map[string]any{
		"token":  "wrong-token",
		"action": "call",
		"tool":   "read_file",
	})
	badPayload = append(badPayload, '\n')
	if _, err := conn.Write(badPayload); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	decoder := json.NewDecoder(conn)
	var respBad map[string]any
	if err := decoder.Decode(&respBad); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if ok, _ := respBad["ok"].(bool); ok {
		t.Fatalf("expected ok=false for bad token, got ok=true")
	}

	// 验证服务端已断开连接（后续读取返回 EOF）
	var dummy map[string]any
	if err := decoder.Decode(&dummy); err == nil {
		t.Fatalf("expected connection to be closed by server on bad token, but read succeeded: %v", dummy)
	}

	// 3. 携带正确 token 应当正常调用
	respGood, errGood := sendBridgeRequestWithToken(srv.SocketPath(), srv.Token(), map[string]any{
		"action": "call",
		"tool":   "read_file",
	})
	if errGood != nil {
		t.Fatalf("expected success with valid token, got err: %v", errGood)
	}
	if ok, _ := respGood["ok"].(bool); !ok {
		t.Fatalf("expected ok=true with valid token, got: %v", respGood["error"])
	}
}

func sendBridgeRequestWithToken(sockPath string, token string, req map[string]any) (map[string]any, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if token != "" {
		req["token"] = token
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if _, err := conn.Write(payload); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(conn)
	var resp map[string]any
	if err := decoder.Decode(&resp); err != nil {
		return nil, err
	}
	return resp, nil
}
