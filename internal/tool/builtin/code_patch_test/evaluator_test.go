package codepatchtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeCodePatchRegistry struct{ executed int }

func (r *fakeCodePatchRegistry) Register(types.Tool) error { return nil }
func (r *fakeCodePatchRegistry) Lookup(name string) (types.Tool, error) {
	if name != "code_patch_test" {
		return types.Tool{}, apperr.New(apperr.CodeNotFound, "missing")
	}
	return types.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, nil
}
func (r *fakeCodePatchRegistry) List() []types.Tool { return nil }
func (r *fakeCodePatchRegistry) ExecuteTool(context.Context, string, []byte, types.TaintLevel) (*types.ToolResult, error) {
	r.executed++
	return &types.ToolResult{Success: true, Output: []byte(`{"passed":true,"phase":"candidate","baseline_fail_to_pass":[{"exit_code":1}],"baseline_pass_to_pass":[{"exit_code":0}],"candidate_fail_to_pass":[{"exit_code":0}],"candidate_pass_to_pass":[{"exit_code":0}]}`)}, nil
}

type codePatchAuthorizer struct {
	allow bool
	args  []byte
	taint types.TaintLevel
}

func (a *codePatchAuthorizer) AuthorizeCall(_ context.Context, _ types.Tool, args []byte, taint types.TaintLevel) (protocol.PermissionGrant, string, bool) {
	a.args = append([]byte(nil), args...)
	a.taint = taint
	if !a.allow {
		return protocol.GrantNone, "needs interactive approval", false
	}
	return protocol.GrantAuto, "approved by policy", true
}

type codePatchCallerFactory struct{}

func (codePatchCallerFactory) ForCodePatchTest(ctx context.Context, _ protocol.EvalCase) (context.Context, func(), error) {
	return protocol.WithPermissionGrant(ctx, protocol.GrantNone), func() {}, nil
}

func TestEvaluatorRequiresFreshHighTaintAuthorizationBeforeExecution(t *testing.T) {
	registry := &fakeCodePatchRegistry{}
	authorizer := &codePatchAuthorizer{}
	evaluator := Evaluator{Registry: registry, Authorizer: authorizer, CallerContexts: codePatchCallerFactory{}}
	caseData := protocol.EvalCase{ID: "auth", Description: "fix", Input: map[string]any{"repo_path": "/tmp/repo", "base_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "test_patch": "trusted"}, Config: map[string]any{"fail_to_pass_commands": []string{"F2P"}, "pass_to_pass_commands": []string{"PTP"}}}
	if passed, err := evaluator.EvaluateCodePatch(context.Background(), caseData, []byte("candidate")); err == nil || passed {
		t.Fatalf("denied authorization was accepted: passed=%t err=%v", passed, err)
	}
	if registry.executed != 0 {
		t.Fatal("tool execution occurred after authorization denial")
	}
	if authorizer.taint != types.TaintHigh {
		t.Fatalf("authorization used taint %v, want High", authorizer.taint)
	}
	var authorizedInput map[string]any
	if err := json.Unmarshal(authorizer.args, &authorizedInput); err != nil {
		t.Fatal(err)
	}
	if authorizedInput["candidate_patch"] != "candidate" || authorizedInput["test_patch"] != "trusted" {
		t.Fatalf("authorizer did not receive the actual tool arguments: %v", authorizedInput)
	}
}
