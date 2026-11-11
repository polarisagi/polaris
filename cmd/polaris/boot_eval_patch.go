package main

import (
	"context"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	codepatchtest "github.com/polarisagi/polaris/internal/tool/builtin/code_patch_test"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const codePatchEvaluatorAgentID = "eval-code-patch"

type codePatchCallerContexts struct{ allowedPaths []string }

func newCodePatchCallerContexts(allowedPaths []string) codePatchCallerContexts {
	return codePatchCallerContexts{allowedPaths: append([]string(nil), allowedPaths...)}
}

func (f codePatchCallerContexts) ForCodePatchTest(ctx context.Context, c protocol.EvalCase) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, apperr.New(apperr.CodeInvalidInput, "code_patch_test: caller context is required")
	}
	path, _ := c.Input["repo_path"].(string)
	if !filepath.IsAbs(path) {
		return nil, nil, apperr.New(apperr.CodeInvalidInput, "code_patch_test: local absolute repo_path is required")
	}
	repo, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInvalidInput, "code_patch_test: resolve repo_path", err)
	}
	if err := guard.CheckScopeRoot(repo); err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeForbidden, "code_patch_test: unsafe repo_path", err)
	}
	// 基准数据只能引用已有授权根；即使 ctx 有更宽的授权凭据，也不扩张路径白名单。
	scopeCtx := protocol.WithPermissionGrant(ctx, protocol.GrantNone)
	if err := guard.CheckReadablePath(scopeCtx, repo, guard.ScopedPaths(scopeCtx, f.allowedPaths)); err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeForbidden, "code_patch_test: repo_path is outside authorized project roots", err)
	}
	callCtx := identityFreeContext{Context: protocol.WithPermissionGrant(ctx, protocol.GrantNone)}
	tok, err := action.NewJITToken(codePatchEvaluatorAgentID, []action.TokenOperation{{ToolName: "code_patch_test", MaxCalls: 1}}, 0)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeForbidden, "code_patch_test: mint one-use capability", err)
	}
	callCtx = identityFreeContext{Context: context.WithValue(callCtx, protocol.CtxCapabilityTokenKey{}, tok)}
	revoke := func() { action.GetTokenManager().Revoke(tok.Claims.TokenID) }
	return callCtx, revoke, nil
}

// identityFreeContext prevents benchmark payloads from entering session/task event logs.
type identityFreeContext struct{ context.Context }

func (c identityFreeContext) Value(key any) any {
	switch key.(type) {
	case protocol.CtxTaskIDKey, protocol.CtxSessionIDKey:
		return nil
	default:
		return c.Context.Value(key)
	}
}

func newCodePatchEvaluator(allowedPaths []string, registry protocol.ToolRegistry, authorizer protocol.ToolCallAuthorizer) protocol.CodePatchEvaluator {
	return codepatchtest.Evaluator{Registry: registry, CallerContexts: newCodePatchCallerContexts(allowedPaths), Authorizer: authorizer}
}
