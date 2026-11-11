package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	codepatchtest "github.com/polarisagi/polaris/internal/tool/builtin/code_patch_test"
	"github.com/polarisagi/polaris/pkg/types"
)

type evalPatchTestAuthorizer struct{}

func (evalPatchTestAuthorizer) AuthorizeCall(context.Context, types.Tool, []byte, types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	return protocol.GrantNone, "test only", false
}

func TestCodePatchCallerContextsRequiresExistingAuthorizedRoot(t *testing.T) {
	allowed := filepath.Join(t.TempDir(), "allowed")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, path := range []string{allowed, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx := protocol.WithProjectRoot(context.Background(), allowed)
	factory := newCodePatchCallerContexts([]string{allowed})
	_, _, err := factory.ForCodePatchTest(ctx, protocol.EvalCase{Input: map[string]any{"repo_path": outside}})
	if err == nil {
		t.Fatal("repo_path outside process/context scope was accepted")
	}
}

func TestCodePatchCallerContextsMintsOneUseIdentityFreeToken(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := evalPatchTestAuthorizer{}
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "task-secret")
	ctx = context.WithValue(ctx, protocol.CtxSessionIDKey{}, "session-secret")
	ctx = protocol.WithProjectRoot(ctx, root)
	ctx = protocol.WithToolCallAuthorizer(ctx, authorizer)
	callCtx, revoke, err := newCodePatchCallerContexts([]string{root}).ForCodePatchTest(ctx, protocol.EvalCase{Input: map[string]any{"repo_path": root}})
	if err != nil {
		t.Fatal(err)
	}
	if revoke == nil {
		t.Fatal("missing token revoker")
	}
	if protocol.ProjectRootFrom(callCtx) != root || protocol.ToolCallAuthorizerFromContext(callCtx) != authorizer {
		t.Fatal("caller context lost the authorized root or policy authorizer")
	}
	if protocol.TaskIDFrom(callCtx) != "" || callCtx.Value(protocol.CtxSessionIDKey{}) != nil {
		t.Fatal("benchmark task/session identity could flow into tool events")
	}
	tok, ok := callCtx.Value(protocol.CtxCapabilityTokenKey{}).(*token.Token)
	if !ok || tok == nil {
		t.Fatal("missing one-use capability token")
	}
	if err := action.GetTokenManager().Verify(tok); err != nil {
		t.Fatalf("minted capability token is invalid: %v", err)
	}
	revoke()
	if err := action.GetTokenManager().Verify(tok); err == nil {
		t.Fatal("revoked capability token remained valid")
	}
}

var _ protocol.ToolCallAuthorizer = evalPatchTestAuthorizer{}
var _ protocol.CodePatchEvaluator = codepatchtest.Evaluator{}
