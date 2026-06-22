package authcontext

import (
	"context"
)

type contextKey string

const (
	authContextKey contextKey = "polaris_auth_context"
)

// AuthContext 封装了经过认证的客户端身份信息
type AuthContext struct {
	UserID     string
	ClientType string // e.g., "cli", "webui", "api"
	TraceID    string // 全链路请求唯一追踪 ID
}

// WithAuthContext 将鉴权上下文注入请求 context 中
func WithAuthContext(ctx context.Context, auth *AuthContext) context.Context {
	return context.WithValue(ctx, authContextKey, auth)
}

// FromContext 尝试从请求 context 中提取鉴权上下文
// 如果未找到（如内部直接调用或健康检查），返回一个匿名的 default context
func FromContext(ctx context.Context) *AuthContext {
	val := ctx.Value(authContextKey)
	if auth, ok := val.(*AuthContext); ok {
		return auth
	}
	return &AuthContext{
		UserID:     "anonymous",
		ClientType: "unknown",
	}
}
