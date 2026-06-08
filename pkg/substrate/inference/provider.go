package inference

import (
	"context"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ProviderAdapter 适配器基类，实现通用功能 (API Key JIT, 错误包装等)。
// 本文件实现了 M1 Inference Runtime 的 Provider 接口。
type ProviderAdapter struct {
	id           string
	capabilities protocol.ProviderCapabilities
	tokenizer    protocol.TokenizerAdapter
}

func (p *ProviderAdapter) ModelID() string {
	return p.id
}

func (p *ProviderAdapter) Capabilities() protocol.ProviderCapabilities {
	return p.capabilities
}

func (p *ProviderAdapter) Tokenizer() protocol.TokenizerAdapter {
	return p.tokenizer
}

// Ensure interface compliance
var _ protocol.Provider = (*ProviderAdapter)(nil)

// Infer implements the non-streaming inference call.
// Each specific adapter (e.g. OpenAI/Anthropic) should embed ProviderAdapter and override this.
func (p *ProviderAdapter) Infer(ctx context.Context, req *protocol.InferRequest) (*protocol.InferResponse, error) {
	return nil, perrors.New(perrors.CodeInternal, "not implemented")
}

// StreamInfer implements the streaming inference call.
func (p *ProviderAdapter) StreamInfer(ctx context.Context, req *protocol.InferRequest) (<-chan protocol.StreamEvent, error) {
	return nil, perrors.New(perrors.CodeInternal, "not implemented")
}
