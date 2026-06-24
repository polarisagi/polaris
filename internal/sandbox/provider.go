package sandbox

import (
	"github.com/polarisagi/polaris/internal/security/token"
)

// TokenVerifier 定义 Token 验证接口（Consumer-side Interface）。
// 供 ExecEnvelope 在隔离环境中校验调用方能力凭证，打破对 pkg/action 或安全包的直接依赖。
type TokenVerifier interface {
	Verify(t *token.Token) error
}

// 占位接口：未来可以扩展更多 Consumer-side 接口以切断循环依赖，例如 ASTChecker 等。
