package llm

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ProviderFactory 负责按 providerID 和 modelID 动态构建 protocol.Provider 实例。
// 由应用层（如 cmd/polaris）注入，避免 internal/llm 反向依赖外层。
type ProviderFactory interface {
	CreateProvider(ctx context.Context, providerID, modelID string) (protocol.Provider, string, error)
}
