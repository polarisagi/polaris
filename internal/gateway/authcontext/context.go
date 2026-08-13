package authcontext

import (
	"context"
)

type contextKey string

const (
	authContextKey contextKey = "polaris_auth_context"
)

type ClientType string

const (
	ClientTypeUnknown    ClientType = "unknown"
	ClientTypeWebUI      ClientType = "webui"
	ClientTypeLocalWebUI ClientType = "local_webui"
	ClientTypeLocal      ClientType = "local"
	ClientTypeAPI        ClientType = "api"
	ClientTypeSDK        ClientType = "sdk"
	ClientTypeCLI        ClientType = "cli"
)

// AuthContext 封装了经过认证的客户端身份信息
type AuthContext struct {
	UserID        string
	ClientType    ClientType // e.g., "cli", "webui", "api"
	TraceID       string     // 全链路请求唯一追踪 ID
	Authenticated bool       // 是否通过了有效凭证校验（如 API Key）
}

// WithAuthContext 将鉴权上下文注入请求 context 中
func WithAuthContext(ctx context.Context, auth *AuthContext) context.Context {
	return context.WithValue(ctx, authContextKey, auth)
}

// FromContext 尝试从请求 context 中提取鉴权上下文。
// 注意：本函数永不返回 nil，未找到时返回 UserID:"anonymous", Authenticated:false 的结构体。
// 调用方不得以 nil 判定未认证，必须读取 Authenticated 字段。
func FromContext(ctx context.Context) *AuthContext {
	val := ctx.Value(authContextKey)
	if auth, ok := val.(*AuthContext); ok {
		return auth
	}
	return &AuthContext{
		UserID:        "anonymous",
		ClientType:    ClientTypeUnknown,
		Authenticated: false,
	}
}

// IsLocalTrusted 判定当前客户端是否为本地可信客户端。
func (c ClientType) IsLocalTrusted() bool {
	return c == ClientTypeLocalWebUI || c == ClientTypeWebUI || c == ClientTypeLocal
}
