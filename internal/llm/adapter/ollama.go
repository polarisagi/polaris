package adapter

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"net/http"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// OllamaAdapter 对接本地 Ollama 服务（OpenAI 兼容接口 + /api/chat）。
// Ollama 默认监听 http://localhost:11434，FeatureLocalInference 门控。
type OllamaAdapter struct {
	model   string
	baseURL string
	client  *OpenAICompatibleClient
	caps    types.ProviderCapabilities
	tbr     *metrics.TokenBurnRate
}

var _ protocol.Provider = (*OllamaAdapter)(nil)

// NewOllamaAdapter 构造 Ollama 本地推理适配器。
func NewOllamaAdapter(model string, httpClient *http.Client, tbr *metrics.TokenBurnRate) *OllamaAdapter {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	baseURL := "http://localhost:11434/v1"
	return &OllamaAdapter{
		model:   model,
		baseURL: baseURL,
		client: &OpenAICompatibleClient{
			BaseURL:    baseURL,
			HTTPClient: httpClient,
		},
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsThinking:  false,
			MaxContextTokens:  32768,
			CostPer1KInput:    0.0,
			CostPer1KOutput:   0.0,
		},
		tbr: tbr,
	}
}

func (a *OllamaAdapter) ModelID() string                          { return a.model }
func (a *OllamaAdapter) Capabilities() types.ProviderCapabilities { return a.caps }

func (a *OllamaAdapter) Tokenizer() protocol.TokenizerAdapter { return &llmparent.SimpleTokenizer{} }

func (a *OllamaAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &types.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}
	apiReq := translateRequest(req, a.caps.SupportsVision)
	apiReq.Model = a.model
	apiReq.Stream = false

	resp, err := a.client.SendRequest(ctx, nil, apiReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ollama infer", err)
	}

	out := &types.ProviderResponse{
		Model: a.model,
		Usage: types.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	if len(resp.Choices) > 0 {
		contentStr, _ := resp.Choices[0].Message.Content.(string)
		out.Content = contentStr
	}
	if out.Usage.InputTokens > 0 || out.Usage.OutputTokens > 0 {
		if a.tbr != nil {
			a.tbr.Add(int64(out.Usage.InputTokens + out.Usage.OutputTokens))
		}
	}
	return out, nil
}

func (a *OllamaAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &types.InferRequest{
		Messages:        msgs,
		Model:           options.Model,
		MaxTokens:       options.MaxTokens,
		Tools:           options.Tools,
		ThinkingMode:    options.ThinkingMode,
		Temperature:     options.Temperature,
		ResponseFormat:  options.ResponseFormat,
		ReasoningEffort: options.ReasoningEffort,
	}
	apiReq := translateRequest(req, a.caps.SupportsVision)
	apiReq.Model = a.model
	tok := &llmparent.SimpleTokenizer{}
	return a.client.SendStreamRequest(ctx, nil, apiReq, tok.EstimateRequestTokens(req))
}
