package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// ToolCallAuthorizer 对 code_act 工具桥和执行前 Hook 改参调用做统一权限判定
// （ADR-0121 决策六）。实现须先校验 tool.InputSchema，再按本次 args/taint 执行
// policy.Decide；不允许把外层调用的 grant 作为内层授权。ok=true 时 grant 是该次
// 参数的授权凭据；ok=false 时 reason 可返回给调用方。Ask 必须返回 ok=false。
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
