package codepatchtest

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type Evaluator struct {
	Registry       protocol.ToolRegistry
	Authorizer     protocol.ToolCallAuthorizer
	CallerContexts CodePatchCallerContextFactory
}

type CodePatchCallerContextFactory interface {
	ForCodePatchTest(context.Context, protocol.EvalCase) (context.Context, func(), error)
}

func (e Evaluator) EvaluateCodePatch(ctx context.Context, c protocol.EvalCase, candidate []byte) (bool, error) {
	if e.Registry == nil || e.Authorizer == nil || e.CallerContexts == nil || len(candidate) == 0 {
		return false, apperr.New(apperr.CodeServiceUnavailable, "code_patch_test evaluator: registry, real authorizer, caller context factory, and candidate required")
	}
	callCtx, revoke, err := e.CallerContexts.ForCodePatchTest(ctx, c)
	if err != nil {
		return false, apperr.Wrap(apperr.CodeForbidden, "code_patch_test evaluator: mint tool caller context", err)
	}
	if callCtx == nil || revoke == nil {
		return false, apperr.New(apperr.CodeServiceUnavailable, "code_patch_test evaluator: incomplete caller context")
	}
	defer revoke()
	toolResult, err := e.executeCodePatchTool(callCtx, c, candidate)
	if err != nil {
		return false, err
	}
	var evaluated result
	if err := json.Unmarshal(toolResult.Output, &evaluated); err != nil {
		return false, apperr.Wrap(apperr.CodeInternal, "code_patch_test evaluator: parse result", err)
	}
	passed, evidenceOK := validateResult(evaluated)
	if !evidenceOK {
		return false, apperr.New(apperr.CodeInternal, "code_patch_test evaluator: incomplete or inconsistent execution evidence")
	}
	return passed, nil
}

func (e Evaluator) executeCodePatchTool(ctx context.Context, c protocol.EvalCase, candidate []byte) (*types.ToolResult, error) {
	input, err := buildToolInput(c, candidate)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test evaluator: encode input", err)
	}
	tool, err := e.Registry.Lookup("code_patch_test")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, "code_patch_test evaluator: lookup tool", err)
	}
	grant, reason, ok := e.Authorizer.AuthorizeCall(protocol.WithPermissionGrant(ctx, protocol.GrantNone), tool, encoded, types.TaintHigh)
	if !ok || grant == protocol.GrantNone {
		return nil, apperr.New(apperr.CodeForbidden, "code_patch_test evaluator: authorization denied: "+reason)
	}
	toolResult, err := e.Registry.ExecuteTool(protocol.WithPermissionGrant(ctx, grant), "code_patch_test", encoded, types.TaintHigh)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "code_patch_test evaluator: execute tool", err)
	}
	if toolResult == nil || !toolResult.Success {
		return nil, apperr.New(apperr.CodeInternal, "code_patch_test evaluator: tool execution failed")
	}
	return toolResult, nil
}

func buildToolInput(c protocol.EvalCase, candidate []byte) (map[string]any, error) {
	path, _ := c.Input["repo_path"].(string)
	baseCommit, _ := c.Input["base_commit"].(string)
	testPatch, _ := c.Input["test_patch"].(string)
	failToPass, err := stringList(c.Config["fail_to_pass_commands"])
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test evaluator: invalid fail_to_pass_commands", err)
	}
	passToPass, err := stringList(c.Config["pass_to_pass_commands"])
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test evaluator: invalid pass_to_pass_commands", err)
	}
	if path == "" || baseCommit == "" || len(failToPass) == 0 || len(passToPass) == 0 {
		return nil, apperr.New(apperr.CodeUnimplemented, "code_patch_test evaluator: unsupported without local repo, full base_commit, and explicit test command groups")
	}
	return map[string]any{
		"path": path, "base_commit": baseCommit, "candidate_patch": string(candidate), "test_patch": testPatch,
		"fail_to_pass_commands": failToPass, "pass_to_pass_commands": passToPass,
	}, nil
}

func validateResult(r result) (bool, bool) {
	if !allCommandsFail(r.BaselineF2P) || !allCommandsPass(r.BaselinePTP) {
		return false, r.Phase == "baseline" && !r.Passed
	}
	if r.Phase != "candidate" || len(r.CandidateF2P) == 0 || len(r.CandidatePTP) == 0 {
		return false, false
	}
	passed := allCommandsPass(r.CandidateF2P) && allCommandsPass(r.CandidatePTP)
	return passed, r.Passed == passed
}

func stringList(value any) ([]string, error) {
	switch items := value.(type) {
	case []string:
		return items, nil
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, apperr.New(apperr.CodeInvalidInput, "expected string list")
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, apperr.New(apperr.CodeInvalidInput, "expected string list")
	}
}
