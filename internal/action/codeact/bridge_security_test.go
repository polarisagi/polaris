package codeact_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type callRecord struct {
	toolName string
	input    string
	taint    types.TaintLevel
}

type recordingToolExecutor struct {
	mu      sync.Mutex
	calls   []callRecord
	tools   map[string]types.Tool
	returns map[string]*types.ToolResult
}

func (r *recordingToolExecutor) Lookup(name string) (types.Tool, error) {
	t, ok := r.tools[name]
	if !ok {
		return types.Tool{}, apperr.New(apperr.CodeNotFound, "tool not found")
	}
	return t, nil
}

func (r *recordingToolExecutor) List() []types.Tool {
	out := make([]types.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

func (r *recordingToolExecutor) ExecuteTool(_ context.Context, name string, input []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, callRecord{
		toolName: name,
		input:    string(input),
		taint:    taintLevel,
	})
	if res, ok := r.returns[name]; ok {
		return res, nil
	}
	return &types.ToolResult{Success: true, Output: []byte(`{"status":"ok"}`), TaintLevel: taintLevel}, nil
}

// TestV5_Bridge_SubcallsRouteThroughExecuteTool 验证通过 Bridge 发起的每次子工具调用
// 严格且确定性地走 ExecuteTool，无旁路逃逸（V5.1 / HE-2）。
func TestV5_Bridge_SubcallsRouteThroughExecuteTool(t *testing.T) {
	rec := &recordingToolExecutor{
		tools: map[string]types.Tool{
			"read_file": {
				Name:        "read_file",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
		},
		returns: map[string]*types.ToolResult{
			"read_file": {Success: true, Output: []byte(`{"content":"test data"}`), TaintLevel: types.TaintLow},
		},
	}

	srv, err := codeact.StartBridgeServer(context.Background(), rec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatalf("StartBridgeServer failed: %v", err)
	}
	defer srv.Close()

	resp, err := sendBridgeRequest(srv.SocketPath(), map[string]any{
		"action": "call",
		"tool":   "read_file",
		"args":   map[string]any{"path": "data.txt"},
	})
	if err != nil {
		t.Fatalf("sendBridgeRequest failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got error: %v", resp["error"])
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly 1 ExecuteTool call, got %d", len(rec.calls))
	}
	call := rec.calls[0]
	if call.toolName != "read_file" {
		t.Errorf("expected toolName 'read_file', got %s", call.toolName)
	}
	if !strings.Contains(call.input, "data.txt") {
		t.Errorf("expected input to contain 'data.txt', got %s", call.input)
	}
	if call.taint != types.TaintLow {
		t.Errorf("expected taint TaintLow, got %v", call.taint)
	}
}

// TestV5_Bridge_SocketPermissionsAndRandomPath 验证 Bridge Unix Socket 权限为 0600
// 且路径随机不可预测（V5.4 / HE-2）。
func TestV5_Bridge_SocketPermissionsAndRandomPath(t *testing.T) {
	rec := &recordingToolExecutor{tools: make(map[string]types.Tool)}

	srv1, err := codeact.StartBridgeServer(context.Background(), rec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatalf("StartBridgeServer 1 failed: %v", err)
	}
	defer srv1.Close()

	srv2, err := codeact.StartBridgeServer(context.Background(), rec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatalf("StartBridgeServer 2 failed: %v", err)
	}
	defer srv2.Close()

	// 1. 权限必须为 0600
	info, err := os.Stat(srv1.SocketPath())
	if err != nil {
		t.Fatalf("stat socket failed: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Fatalf("expected socket permissions 0600, got %o", perm)
	}

	// 2. 两个服务的 socket 路径必须独立随机，不可预测
	if srv1.SocketPath() == srv2.SocketPath() {
		t.Fatalf("expected different random socket paths, both got %s", srv1.SocketPath())
	}
	if !strings.HasPrefix(srv1.SocketPath(), "/tmp/pb_") {
		t.Errorf("expected /tmp/pb_ prefix, got %s", srv1.SocketPath())
	}
}

// TestV5_Bridge_StandardProfile_BlocksWritingTools 验证 standard 档下拒绝写类工具（V5.3）。
func TestV5_Bridge_StandardProfile_BlocksWritingTools(t *testing.T) {
	rec := &recordingToolExecutor{
		tools: map[string]types.Tool{
			"rm_file": {
				Name:        "rm_file",
				Capability:  types.CapWriteLocal,
				SideEffects: []types.SideEffect{types.SideFileWrite},
			},
		},
	}

	srv, err := codeact.StartBridgeServer(context.Background(), rec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatalf("StartBridgeServer failed: %v", err)
	}
	defer srv.Close()

	resp, err := sendBridgeRequest(srv.SocketPath(), map[string]any{
		"action": "call",
		"tool":   "rm_file",
		"args":   map[string]any{"path": "target.txt"},
	})
	if err != nil {
		t.Fatalf("sendBridgeRequest failed: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("expected write tool to be blocked in standard profile, but succeeded")
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "not CapReadOnly") && !strings.Contains(errMsg, "blocked in standard") {
		t.Fatalf("expected error mentioning blocked capability, got: %s", errMsg)
	}
}

// TestV5_Bridge_TaintMerging 验证子工具产生的污点提升并入 HighestTaint（V5.2 / HE-2）。
func TestV5_Bridge_TaintMerging(t *testing.T) {
	rec := &recordingToolExecutor{
		tools: map[string]types.Tool{
			"untrusted_read": {
				Name:        "untrusted_read",
				Capability:  types.CapReadOnly,
				SideEffects: []types.SideEffect{types.SideNone},
			},
		},
		returns: map[string]*types.ToolResult{
			"untrusted_read": {
				Success:    true,
				Output:     []byte(`{"data":"tainted"}`),
				TaintLevel: types.TaintHigh,
			},
		},
	}

	srv, err := codeact.StartBridgeServer(context.Background(), rec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatalf("StartBridgeServer failed: %v", err)
	}
	defer srv.Close()

	if srv.HighestTaint() != types.TaintLow {
		t.Fatalf("expected initial taint TaintLow, got %v", srv.HighestTaint())
	}

	_, _ = sendBridgeRequest(srv.SocketPath(), map[string]any{
		"action": "call",
		"tool":   "untrusted_read",
		"args":   map[string]any{},
	})

	if srv.HighestTaint() != types.TaintHigh {
		t.Fatalf("expected HighestTaint to be elevated to TaintHigh, got %v", srv.HighestTaint())
	}
}
