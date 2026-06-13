package inference

import (
	"context"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// OpenAIAdapter 实现 protocol.Provider，对接官方 OpenAI 或任何严格兼容 OpenAI API 的服务。
// 复用了 client.go 中通用的 OpenAICompatibleClient。
type OpenAIAdapter struct {
	model        string
	credentialFn func() string
	client       *OpenAICompatibleClient
	caps         protocol.ProviderCapabilities
}

var _ protocol.Provider = (*OpenAIAdapter)(nil)

// NewOpenAIAdapter 初始化一个 OpenAI 适配器。
// baseURL 默认为 "https://api.openai.com/v1"（如果传入空串）。
func NewOpenAIAdapter(baseURL, model string, credFn func() string, client *http.Client) *OpenAIAdapter {
	if client == nil {
		client = defaultHTTPClient
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	c := &OpenAICompatibleClient{
		BaseURL:    baseURL,
		HTTPClient: client,
	}

	return &OpenAIAdapter{
		model:        model,
		credentialFn: credFn,
		client:       c,
		caps: protocol.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			// gpt-4o / gpt-4o-mini 等均支持视觉输入；client.go parseImagePart 已实现
			// 此处声明后路由层才能在多模态请求时将 OpenAI 纳入候选
			SupportsVision:   true,
			MaxContextTokens: 128000,
			CostPer1KInput:   0.15,
			CostPer1KOutput:  0.60,
		},
	}
}

func (a *OpenAIAdapter) ModelID() string {
	return a.model
}

func (a *OpenAIAdapter) Capabilities() protocol.ProviderCapabilities {
	return a.caps
}

func (a *OpenAIAdapter) Tokenizer() protocol.TokenizerAdapter {
	return newTiktokenTokenizer(a.model)
}

func (a *OpenAIAdapter) Infer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (*protocol.ProviderResponse, error) {
	options := &protocol.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &protocol.InferRequest{
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
	apiReq.Model = resolveOpenAIModel(a.model)
	if req.Model != "" {
		apiReq.Model = resolveOpenAIModel(req.Model)
	}

	apiKey := a.credentialFn()
	defer clearString(&apiKey)

	resp, err := a.client.SendRequest(ctx, apiKey, apiReq)
	if err != nil {
		return nil, err
	}

	out := &protocol.ProviderResponse{
		Model: resp.ID,
		Usage: protocol.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}

	if out.Usage.InputTokens > 0 || out.Usage.OutputTokens > 0 {
		observability.GlobalTokenBurnRate.Add(int64(out.Usage.InputTokens + out.Usage.OutputTokens))
	}

	if len(resp.Choices) > 0 {
		contentStr, _ := resp.Choices[0].Message.Content.(string)
		out.Content = contentStr
		out.FinishReason = resp.Choices[0].FinishReason
		for _, tc := range resp.Choices[0].Message.ToolCalls {
			input := []byte(tc.Function.Arguments)
			if len(input) == 0 {
				input = []byte("{}")
			}
			out.ToolCalls = append(out.ToolCalls, protocol.InferToolCall{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	return out, nil
}

func (a *OpenAIAdapter) StreamInfer(ctx context.Context, msgs []protocol.Message, opts ...protocol.InferOption) (<-chan protocol.StreamEvent, error) {
	options := &protocol.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &protocol.InferRequest{
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
	apiReq.Model = resolveOpenAIModel(a.model)
	if req.Model != "" {
		apiReq.Model = resolveOpenAIModel(req.Model)
	}

	apiKey := a.credentialFn()
	defer clearString(&apiKey)

	tok := newTiktokenTokenizer(a.model)
	return a.client.SendStreamRequest(ctx, apiKey, apiReq, tok.EstimateRequest(req))
}

func resolveOpenAIModel(requested string) string {
	switch requested {
	case "gpt-3.5-turbo", "gpt-4":
		return "gpt-4o-mini"
	case "gpt-4-turbo", "gpt-4-turbo-preview":
		return "gpt-4o"
	default:
		if requested == "" {
			return "gpt-4o-mini"
		}
		return requested
	}
}
