package adapter

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"net/http"
	"strings"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// OpenAIAdapter 实现 protocol.Provider，对接官方 OpenAI 或任何严格兼容 OpenAI API 的服务。
// 复用了 client.go 中通用的 OpenAICompatibleClient。
type OpenAIAdapter struct {
	model        string
	credentialFn func() []byte
	client       *OpenAICompatibleClient
	caps         types.ProviderCapabilities
	tbr          *metrics.TokenBurnRate
}

var _ protocol.Provider = (*OpenAIAdapter)(nil)

// NewOpenAIAdapter 初始化一个 OpenAI 适配器。
// baseURL 默认为 "https://api.openai.com/v1"（如果传入空串）。
func NewOpenAIAdapter(baseURL, model string, credFn func() []byte, client *http.Client, tbr *metrics.TokenBurnRate) *OpenAIAdapter {
	if client == nil {
		client = defaultHTTPClient()
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
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			// gpt-4o / gpt-4o-mini 等均支持视觉输入；client.go parseImagePart 已实现
			// 此处声明后路由层才能在多模态请求时将 OpenAI 纳入候选
			SupportsVision:   true,
			MaxContextTokens: 128000,
			CostPer1KInput:   0.15,
			CostPer1KOutput:  0.60,
		},
		tbr: tbr,
	}
}

func (a *OpenAIAdapter) ModelID() string {
	return a.model
}

func (a *OpenAIAdapter) Capabilities() types.ProviderCapabilities {
	return a.caps
}

func (a *OpenAIAdapter) Tokenizer() protocol.TokenizerAdapter {
	return llmparent.NewTiktokenTokenizer(a.model)
}

func (a *OpenAIAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultInferTimeout)
		defer cancel()
	}
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
	apiReq.Model = resolveOpenAIModel(a.model)
	if req.Model != "" {
		apiReq.Model = resolveOpenAIModel(req.Model)
	}

	apiKey := a.credentialFn()
	defer llmparent.ClearBytes(apiKey)

	resp, err := a.client.SendRequest(ctx, apiKey, apiReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "OpenAIAdapter.Infer", err)
	}

	out := &types.ProviderResponse{
		Model: resp.ID,
		Usage: types.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}

	if resp.Usage.PromptTokensDetails != nil {
		out.Usage.CacheHitTokens = resp.Usage.PromptTokensDetails.CachedTokens
	}

	if out.Usage.InputTokens > 0 || out.Usage.OutputTokens > 0 {
		if a.tbr != nil {
			a.tbr.Add(int64(out.Usage.InputTokens + out.Usage.OutputTokens))
		}
	}

	metrics.RecordLLMCacheHit("openai", req.Model, out.Usage.CacheHitTokens > 0)

	if len(resp.Choices) > 0 {
		contentStr, _ := resp.Choices[0].Message.Content.(string)
		out.Content = contentStr
		out.FinishReason = resp.Choices[0].FinishReason
		for _, tc := range resp.Choices[0].Message.ToolCalls {
			input := []byte(tc.Function.Arguments)
			if len(input) == 0 {
				input = []byte("{}")
			}
			out.ToolCalls = append(out.ToolCalls, types.InferToolCall{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	return out, nil
}

func (a *OpenAIAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultStreamInferTimeout)
		defer cancel()
	}
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
	apiReq.Model = resolveOpenAIModel(a.model)
	if req.Model != "" {
		apiReq.Model = resolveOpenAIModel(req.Model)
	}

	apiKey := a.credentialFn()
	defer llmparent.ClearBytes(apiKey)

	tok := llmparent.NewTiktokenTokenizer(a.model)
	rawCh, err := a.client.SendStreamRequest(ctx, apiKey, apiReq, tok.EstimateRequest(req))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "OpenAIAdapter.StreamInfer", err)
	}

	outCh := make(chan types.StreamEvent, 100)
	// [SafeGo] 计费/缓存指标转发：畸形事件触发 panic 此前会直接崩进程。
	concurrent.SafeGo(ctx, "llm.adapter.openai_stream_relay", func(context.Context) {
		defer close(outCh)
		for ev := range rawCh {
			if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
				if a.tbr != nil {
					a.tbr.Add(int64(ev.Usage.InputTokens + ev.Usage.OutputTokens)) // Stream events usually just have deltas or final usage? Actually SendStreamRequest returns final usage only for Input/Output tokens, wait.
				}
			}
			if ev.Usage.CacheHitTokens > 0 || ev.Usage.InputTokens > 0 {
				metrics.RecordLLMCacheHit("openai", req.Model, ev.Usage.CacheHitTokens > 0)
			}
			outCh <- ev
		}
	})
	return outCh, nil
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
