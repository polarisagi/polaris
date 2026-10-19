package protocol

import (
	"context"
)

// ToolProfileChecker 校验 Agent 角色工具调用限制（R2-15）。
type ToolProfileChecker interface {
	CheckProfileTool(toolName, delegateTarget string) error
}

type profileCheckerContextKey struct{}

// WithProfileChecker 将 ToolProfileChecker 注入 Context（R2-15）。
func WithProfileChecker(ctx context.Context, checker ToolProfileChecker) context.Context {
	return context.WithValue(ctx, profileCheckerContextKey{}, checker)
}

// ProfileCheckerFromContext 从 Context 读取 ToolProfileChecker（R2-15）。
func ProfileCheckerFromContext(ctx context.Context) ToolProfileChecker {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(profileCheckerContextKey{}).(ToolProfileChecker); ok {
		return v
	}
	return nil
}
