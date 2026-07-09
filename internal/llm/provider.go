package llm

import (
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ProviderAdapter 适配器基类，实现通用功能 (API Key JIT, 错误包装等)。
// 不实现 protocol.Provider 完整接口——Infer 和 StreamInfer 必须由具体 Adapter 类型实现。
type ProviderAdapter struct {
	id           string
	capabilities types.ProviderCapabilities
	tokenizer    protocol.TokenizerAdapter
}

func (p *ProviderAdapter) ModelID() string {
	return p.id
}

func (p *ProviderAdapter) Capabilities() types.ProviderCapabilities {
	return p.capabilities
}

func (p *ProviderAdapter) Tokenizer() protocol.TokenizerAdapter {
	return p.tokenizer
}
