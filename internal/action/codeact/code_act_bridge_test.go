package codeact_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockToolRegistryForBridge struct {
	tools map[string]types.Tool
}

func newMockToolRegistry() *mockToolRegistryForBridge {
	return &mockToolRegistryForBridge{
		tools: map[string]types.Tool{
			"read_file": {
				Name:        "read_file",
				Description: "Read file content",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			"grep": {
				Name:        "grep",
				Description: "Search text pattern",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
			"write_file": {
				Name:        "write_file",
				Description: "Write content to file",
				Capability:  types.CapWriteLocal,
				SideEffects: []types.SideEffect{types.SideFileWrite},
			},
			"secret_scanner": {
				Name:        "secret_scanner",
				Description: "Scanner returning tainted secret",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
		},
	}
}

func (m *mockToolRegistryForBridge) Lookup(name string) (types.Tool, error) {
	t, ok := m.tools[name]
	if !ok {
		return types.Tool{}, apperr.New(apperr.CodeNotFound, "tool not found")
	}
	return t, nil
}

func (m *mockToolRegistryForBridge) List() []types.Tool {
	out := make([]types.Tool, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t)
	}
	return out
}

func (m *mockToolRegistryForBridge) ExecuteTool(_ context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	switch name {
	case "read_file":
		var args map[string]any
		_ = json.Unmarshal(input, &args)
		path, _ := args["path"].(string)
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"content":"file content of ` + path + `"}`),
			TaintLevel: taintLevel,
		}, nil
	case "grep":
		var args map[string]any
		_ = json.Unmarshal(input, &args)
		pattern, _ := args["pattern"].(string)
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"matches":["found ` + pattern + ` at line 10"]}`),
			TaintLevel: taintLevel,
		}, nil
	case "write_file":
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"bytes_written":42}`),
			TaintLevel: taintLevel,
		}, nil
	case "secret_scanner":
		// 返回高风险污点
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(`{"secret_found":"token-xyz"}`),
			TaintLevel: types.TaintHigh,
		}, nil
	default:
		return &types.ToolResult{
			Success: false,
			Error:   "unknown tool",
		}, nil
	}
}

func sendBridgeRequest(srv *codeact.BridgeServer, req map[string]any) (map[string]any, error) {
	if srv.Token() != "" {
		req["token"] = srv.Token()
	}
	conn, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

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

// TestBridge_RoundTrip_ReadFileAndGrep 验证通过专用 socket 调用 read_file 与 grep 的往返执行。
func TestBridge_RoundTrip_ReadFileAndGrep(t *testing.T) {
	reg := newMockToolRegistry()
	pol := policy.ProfilePolicy(policy.ProfileStandard)

	srv, err := codeact.StartBridgeServer(context.Background(), reg, pol, types.TaintLow)
	if err != nil {
		t.Fatalf("start bridge server failed: %v", err)
	}
	defer srv.Close()

	// 1. 调用 read_file
	readResp, err := sendBridgeRequest(srv, map[string]any{
		"action": "call",
		"tool":   "read_file",
		"args":   map[string]any{"path": "config.json"},
	})
	if err != nil {
		t.Fatalf("read_file request failed: %v", err)
	}
	if ok, _ := readResp["ok"].(bool); !ok {
		t.Fatalf("read_file returned not ok: %v", readResp["error"])
	}
	resultMap, _ := readResp["result"].(map[string]any)
	if resultMap["content"] != "file content of config.json" {
		t.Errorf("read_file result mismatch: %v", resultMap)
	}

	// 2. 调用 grep
	grepResp, err := sendBridgeRequest(srv, map[string]any{
		"action": "call",
		"tool":   "grep",
		"args":   map[string]any{"pattern": "admin"},
	})
	if err != nil {
		t.Fatalf("grep request failed: %v", err)
	}
	if ok, _ := grepResp["ok"].(bool); !ok {
		t.Fatalf("grep returned not ok: %v", grepResp["error"])
	}
	grepMap, _ := grepResp["result"].(map[string]any)
	matches, _ := grepMap["matches"].([]any)
	if len(matches) != 1 || matches[0] != "found admin at line 10" {
		t.Errorf("grep matches mismatch: %v", matches)
	}
}

// TestBridge_TaintPropagation 验证子调用的高等级污点并入最高污点（Taint 不得静默丢失，HE-2）。
func TestBridge_TaintPropagation(t *testing.T) {
	reg := newMockToolRegistry()
	pol := policy.ProfilePolicy(policy.ProfileStandard)

	srv, err := codeact.StartBridgeServer(context.Background(), reg, pol, types.TaintLow)
	if err != nil {
		t.Fatalf("start bridge server failed: %v", err)
	}
	defer srv.Close()

	if srv.HighestTaint() != types.TaintLow {
		t.Errorf("initial taint should be TaintLow, got %v", srv.HighestTaint())
	}

	// 调用 secret_scanner，返回 TaintSecret
	resp, err := sendBridgeRequest(srv, map[string]any{
		"action": "call",
		"tool":   "secret_scanner",
		"args":   map[string]any{},
	})
	if err != nil {
		t.Fatalf("secret_scanner request failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("secret_scanner not ok: %v", resp["error"])
	}

	if srv.HighestTaint() != types.TaintHigh {
		t.Errorf("highest taint should be elevated to TaintHigh, got %v", srv.HighestTaint())
	}
}

// TestBridge_ProfileRestrictions 验证 standard 档下拒绝写类工具，full 档放行。
func TestBridge_ProfileRestrictions(t *testing.T) {
	reg := newMockToolRegistry()

	// 1. Standard 档：拒绝写类工具 write_file
	stdPol := policy.ProfilePolicy(policy.ProfileStandard)
	stdSrv, err := codeact.StartBridgeServer(context.Background(), reg, stdPol, types.TaintLow)
	if err != nil {
		t.Fatalf("start standard bridge server failed: %v", err)
	}
	defer stdSrv.Close()

	writeResp, err := sendBridgeRequest(stdSrv, map[string]any{
		"action": "call",
		"tool":   "write_file",
		"args":   map[string]any{"path": "test.txt", "data": "hello"},
	})
	if err != nil {
		t.Fatalf("write_file request failed: %v", err)
	}
	if ok, _ := writeResp["ok"].(bool); ok {
		t.Errorf("standard profile should reject write_file, but succeeded: %v", writeResp)
	}

	// 2. Full 档但未装配授权器：同样 fail-closed（不再因档位放行全部工具，ADR-0121）
	fullPol := policy.ProfilePolicy(policy.ProfileFull)
	fullSrv, err := codeact.StartBridgeServer(context.Background(), reg, fullPol, types.TaintLow)
	if err != nil {
		t.Fatalf("start full bridge server failed: %v", err)
	}
	defer fullSrv.Close()

	fullWriteResp, err := sendBridgeRequest(fullSrv, map[string]any{
		"action": "call",
		"tool":   "write_file",
		"args":   map[string]any{"path": "test.txt", "data": "hello"},
	})
	if err != nil {
		t.Fatalf("full write_file request failed: %v", err)
	}
	if ok, _ := fullWriteResp["ok"].(bool); ok {
		t.Errorf("no authorizer: write_file must be rejected even in full profile: %v", fullWriteResp)
	}
}

// TestBridge_CallMany 验证并发调用多工具。
func TestBridge_CallMany(t *testing.T) {
	reg := newMockToolRegistry()
	pol := policy.ProfilePolicy(policy.ProfileFull)

	srv, err := codeact.StartBridgeServer(context.Background(), reg, pol, types.TaintLow)
	if err != nil {
		t.Fatalf("start bridge server failed: %v", err)
	}
	defer srv.Close()

	resp, err := sendBridgeRequest(srv, map[string]any{
		"action": "call_many",
		"calls": []map[string]any{
			{"tool": "read_file", "args": map[string]any{"path": "a.txt"}},
			{"tool": "read_file", "args": map[string]any{"path": "b.txt"}},
			{"tool": "grep", "args": map[string]any{"pattern": "target"}},
		},
	})
	if err != nil {
		t.Fatalf("call_many request failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("call_many not ok: %v", resp["error"])
	}
	results, _ := resp["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

// TestBridge_ToolsListing 验证工具发现。
func TestBridge_ToolsListing(t *testing.T) {
	reg := newMockToolRegistry()
	pol := policy.ProfilePolicy(policy.ProfileStandard)

	srv, err := codeact.StartBridgeServer(context.Background(), reg, pol, types.TaintLow)
	if err != nil {
		t.Fatalf("start bridge server failed: %v", err)
	}
	defer srv.Close()

	resp, err := sendBridgeRequest(srv, map[string]any{
		"action": "tools",
	})
	if err != nil {
		t.Fatalf("tools request failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("tools not ok: %v", resp["error"])
	}
	toolList, _ := resp["tools"].([]any)
	// 在 standard 档下，write_file 应被过滤掉，只返回只读工具
	for _, ti := range toolList {
		tm, _ := ti.(map[string]any)
		if tm["name"] == "write_file" {
			t.Errorf("write_file should not be listed in standard profile: %v", tm)
		}
	}
}
