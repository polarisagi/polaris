package mcp

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
)

// tokenGate 只在执行闸门看到有效能力令牌时放行 tool_execute（与 tool_execute_permit 对 trust<3 的要求一致）。
type tokenGate struct{ mockPolicyGate }

func (*tokenGate) IsAuthorized(_ context.Context, _, action, _ string, ctx map[string]any) (bool, error) {
	if action != "tool_execute" {
		return true, nil
	}
	ok, _ := ctx["capability_token_valid"].(bool)
	return ok, nil
}

type acceptVerifier struct{ revoked map[string]bool }

func (v *acceptVerifier) Verify(t *token.Token) error {
	if t == nil || v.revoked[t.Claims.TokenID] {
		return errors.New("invalid")
	}
	return nil
}

// TestCallTool_MintsTokenOnlyForA2AContractTools trust_tier≤2 的委派目标：契约工具（a2a_delegate 等）
// 由 Worker 代调，须补发一次性令牌才能过闸门，用完即撤销；其它工具名不签发，仍被拒绝。
func TestCallTool_MintsTokenOnlyForA2AContractTools(t *testing.T) {
	inProc := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 2)
	verifier := &acceptVerifier{revoked: map[string]bool{}}
	env := sandbox.NewExecEnvelope(&tokenGate{}, router, 2, runtime.GOOS, verifier).
		WithExecutionAuditor(mcpTestExecutionAuditor{})
	mgr := NewMCPManager(inProc, testSafeHTTP(nil), &mockPolicyGate{})
	mgr.SetEnvelope(env)
	mgr.SetCatalog(newFakeCatalog())

	srv := scriptedToolCallServer(t)
	client := NewMCPClient(MCPClientConfig{ServerName: "desktop", Transport: MCPStreamableHTTP, URL: "https://peer.example/mcp", TrustTier: 2}, testSafeHTTP(srv))
	valid := mgr.registerTools("mesh_desktop", "desktop", client, []MCPTool{
		{Name: "a2a_delegate", InputSchema: []byte(`{"type":"object"}`)},
		{Name: "rm_everything", InputSchema: []byte(`{"type":"object"}`)},
	})
	mgr.mu.Lock()
	mgr.entries["mesh_desktop"] = &mcpEntry{name: "desktop", tools: valid, client: client, cfg: MCPClientConfig{TrustTier: 2}}
	mgr.mu.Unlock()
	ctx := context.Background()

	// 未注入签发器：与修复前一致，被闸门拒绝。
	if _, err := mgr.CallTool(ctx, "mesh_desktop", "a2a_delegate", nil); err == nil {
		t.Fatal("without a minter a tier-2 contract tool call must be denied")
	}

	var minted []string
	mgr.SetDelegationTokenMinter(func(llmTool string) (*token.Token, func(), error) {
		if llmTool != MCPToolName("desktop", "a2a_delegate") {
			t.Fatalf("token must be minted for the exact tool, got %s", llmTool)
		}
		tok := &token.Token{Claims: token.TokenClaims{TokenID: "tok-" + llmTool}}
		minted = append(minted, tok.Claims.TokenID)
		return tok, func() { verifier.revoked[tok.Claims.TokenID] = true }, nil
	})
	if _, err := mgr.CallTool(ctx, "mesh_desktop", "a2a_delegate", nil); err != nil {
		t.Fatalf("contract tool must pass the gate with a minted token: %v", err)
	}
	if len(minted) != 1 || !verifier.revoked[minted[0]] {
		t.Fatalf("token must be minted once and revoked after the call, minted=%v", minted)
	}
	if _, err := mgr.CallTool(ctx, "mesh_desktop", "rm_everything", nil); err == nil {
		t.Fatal("non-contract tools must not get a token")
	}
	if len(minted) != 1 {
		t.Fatal("no token may be minted for non-contract tools")
	}
}
