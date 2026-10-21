package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// ToolCallAuthorizer 对 code_act 工具桥内层调用做统一权限判定（ADR-0121 决策六）。
// ok=true 时 grant 为该次调用的授权凭据；ok=false 时 reason 回给脚本。
type ToolCallAuthorizer interface {
	AuthorizeCall(ctx context.Context, tool types.Tool, args []byte, taint types.TaintLevel) (grant PermissionGrant, reason string, ok bool)
}

type toolCallAuthorizerKey struct{}

// WithToolCallAuthorizer 把授权器随 ctx 带到 code_act 引擎（agent 不 import codeact，同 ProfileChecker 做法）。
func WithToolCallAuthorizer(ctx context.Context, a ToolCallAuthorizer) context.Context {
	return context.WithValue(ctx, toolCallAuthorizerKey{}, a)
}

// ToolCallAuthorizerFromContext 读取授权器；未注入返回 nil。
func ToolCallAuthorizerFromContext(ctx context.Context) ToolCallAuthorizer {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(toolCallAuthorizerKey{}).(ToolCallAuthorizer)
	return a
}
