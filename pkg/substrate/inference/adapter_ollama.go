package inference

import (
	"context"
	"net/http"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// OllamaAdapter 对接本地 Ollama 服务（OpenAI 兼容接口 + /api/chat）。
// Ollama 默认监听 http://localhost:11434，FeatureLocalInference 门控。
type OllamaAdapter struct {
	model   string
	baseURL string
	client  *OpenAICompatibleClient
	caps    protocol.ProviderCapabilities
}

var _ protocol.Provider = (*OllamaAdapter)(nil)

// NewOllamaAdapter 构造 Ollama 本地推理适配器。
func NewOllamaAdapter(model string, httpClient *http.Client) *OllamaAdapter {
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	baseURL := "http://localhost:11434/v1"
	return &OllamaAdapter{
		model:   model,
		baseURL: baseURL,
		client: &OpenAICompatibleClient{
			BaseURL:    baseURL,
			HTTPClient: httpClient,
		},
		caps: protocol.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsThinking:  false,
			MaxContextTokens:  32768,
			CostPer1KInput:    0.0,
			CostPer1KOutput:   0.0,
		},
	}
}

func (a *OllamaAdapter) ModelID() string                             { return a.model }
func (a *OllamaAdapter) Capabilities() protocol.ProviderCapabilities { return a.caps }

func (a *OllamaAdapter) Tokenizer() protocol.TokenizerAdapter { return &simpleTokenizer{} }

func (a *OllamaAdapter) Infer(ctx context.Context, req *protocol.InferRequest) (*protocol.InferResponse, error) {
	apiReq := translateRequest(req, a.caps.SupportsVision)
	apiReq.Model = a.model
	apiReq.Stream = false

	resp, err := a.client.SendRequest(ctx, "", apiReq)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "ollama infer", err)
	}

	out := &protocol.InferResponse{
		Model: a.model,
		Usage: protocol.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	if len(resp.Choices) > 0 {
		contentStr, _ := resp.Choices[0].Message.Content.(string)
		out.Content = contentStr
	}
	if out.Usage.InputTokens > 0 || out.Usage.OutputTokens > 0 {
		observability.GlobalTokenBurnRate.Add(int64(out.Usage.InputTokens + out.Usage.OutputTokens))
	}
	return out, nil
}

func (a *OllamaAdapter) StreamInfer(ctx context.Context, req *protocol.InferRequest) (<-chan protocol.StreamEvent, error) {
	apiReq := translateRequest(req, a.caps.SupportsVision)
	apiReq.Model = a.model
	tok := &simpleTokenizer{}
	return a.client.SendStreamRequest(ctx, "", apiReq, tok.estimateRequestTokens(req))
}
