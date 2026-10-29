package codeact_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeAuthorizer struct {
	grant  protocol.PermissionGrant
	reason string
	ok     bool
}

func (f fakeAuthorizer) AuthorizeCall(context.Context, types.Tool, []byte, types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	return f.grant, f.reason, f.ok
}

// grantCapturingExecutor 记录 ExecuteTool 收到的 ctx 凭据。
type grantCapturingExecutor struct {
	recordingToolExecutor
	gmu    sync.Mutex
	grants []protocol.PermissionGrant
}

func (g *grantCapturingExecutor) ExecuteTool(ctx context.Context, name string, input []byte, taint types.TaintLevel) (*types.ToolResult, error) {
	g.gmu.Lock()
	g.grants = append(g.grants, protocol.PermissionGrantFrom(ctx))
	g.gmu.Unlock()
	return g.recordingToolExecutor.ExecuteTool(ctx, name, input, taint)
}

func newWriteToolExec() *grantCapturingExecutor {
	return &grantCapturingExecutor{recordingToolExecutor: recordingToolExecutor{
		tools: map[string]types.Tool{"write_file": {Name: "write_file", Capability: types.CapWriteLocal}},
	}}
}

func callWrite(t *testing.T, srv *codeact.BridgeServer) map[string]any {
	t.Helper()
	resp, err := sendBridgeRequest(srv, map[string]any{"action": "call", "tool": "write_file", "args": map[string]any{"path": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestBridge_AuthorizerAllowOverridesOuterGrant(t *testing.T) {
	// 外层 ctx 带 Explicit，授权器判 Auto：内层执行必须拿到 Auto，不得继承外层。
	outer := protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit)
	exec := newWriteToolExec()
	srv, err := codeact.StartBridgeServer(outer, exec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow,
		codeact.WithBridgeCallAuthorizer(fakeAuthorizer{grant: protocol.GrantAuto, ok: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if ok, _ := callWrite(t, srv)["ok"].(bool); !ok {
		t.Fatal("authorizer allow should execute")
	}
	if len(exec.grants) != 1 || exec.grants[0] != protocol.GrantAuto {
		t.Fatalf("inner grant must be Auto, got %v", exec.grants)
	}
}

func TestBridge_AuthorizerExplicitAndDeny(t *testing.T) {
	exec := newWriteToolExec()
	srv, err := codeact.StartBridgeServer(context.Background(), exec, policy.ProfilePolicy(policy.ProfileFull), types.TaintLow,
		codeact.WithBridgeCallAuthorizer(fakeAuthorizer{grant: protocol.GrantExplicit, ok: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if ok, _ := callWrite(t, srv)["ok"].(bool); !ok || exec.grants[0] != protocol.GrantExplicit {
		t.Fatalf("explicit grant expected, grants=%v", exec.grants)
	}

	exec2 := newWriteToolExec()
	srv2, err := codeact.StartBridgeServer(context.Background(), exec2, policy.ProfilePolicy(policy.ProfileFull), types.TaintLow,
		codeact.WithBridgeCallAuthorizer(fakeAuthorizer{reason: "outside workspace", ok: false}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	resp := callWrite(t, srv2)
	msg, _ := resp["error"].(string)
	if ok, _ := resp["ok"].(bool); ok || !strings.Contains(msg, "outside workspace") || !strings.Contains(msg, "code_act 之外") {
		t.Fatalf("deny expected with reason, got %v", resp)
	}
	if len(exec2.grants) != 0 {
		t.Fatal("denied call must not reach ExecuteTool")
	}
}

func TestBridge_NoAuthorizerWritesGrantNone(t *testing.T) {
	outer := protocol.WithPermissionGrant(context.Background(), protocol.GrantExplicit)
	exec := &grantCapturingExecutor{recordingToolExecutor: recordingToolExecutor{
		tools: map[string]types.Tool{"read_file": {Name: "read_file", Capability: types.CapReadOnly, SideEffects: []types.SideEffect{types.SideNone}}},
	}}
	srv, err := codeact.StartBridgeServer(outer, exec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	resp, _ := sendBridgeRequest(srv, map[string]any{"action": "call", "tool": "read_file", "args": map[string]any{"path": "a"}})
	if ok, _ := resp["ok"].(bool); !ok || exec.grants[0] != protocol.GrantNone {
		t.Fatalf("no authorizer: read ok with GrantNone expected, resp=%v grants=%v", resp, exec.grants)
	}
}

func TestBridge_ToolsListWithAuthorizerIncludesWriters(t *testing.T) {
	exec := newWriteToolExec()
	srv, err := codeact.StartBridgeServer(context.Background(), exec, policy.ProfilePolicy(policy.ProfileStandard), types.TaintLow,
		codeact.WithBridgeCallAuthorizer(fakeAuthorizer{ok: false}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	resp, _ := sendBridgeRequest(srv, map[string]any{"action": "tools"})
	if l, _ := resp["tools"].([]any); len(l) != 1 {
		t.Fatalf("authorizer present: all tools listed, got %v", resp)
	}
}
