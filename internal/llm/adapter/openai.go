package adapter

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"crypto/sha256"
	"encoding/hex"
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
	model    string
	credPool *llmparent.CredentialPool
	client   *OpenAICompatibleClient
	caps     types.ProviderCapabilities
	tbr      *metrics.TokenBurnRate
	// cacheRetention 对应 prompt_cache_retention（in_memory|24h）；空=不发送。
	// 仅在 caps.SupportsPromptCacheKey 为真时才会下发（ADR-0105 决策三）。
	cacheRetention string
}

var _ protocol.Provider = (*OpenAIAdapter)(nil)

// OpenAIOption OpenAIAdapter 可选项。
type OpenAIOption func(*OpenAIAdapter)

// WithOpenAIPromptCacheRetention 设置 prompt_cache_retention（"in_memory"|"24h"，其他值忽略）。
// 只有端点声明支持 prompt_cache_key 时才会被发送。
func WithOpenAIPromptCacheRetention(retention string) OpenAIOption {
	return func(a *OpenAIAdapter) {
		if retention == "in_memory" || retention == "24h" {
			a.cacheRetention = retention
		}
	}
}

// WithOpenAIPromptCacheKey 显式声明端点是否认识 prompt_cache_key/prompt_cache_retention。
// 默认仅 OpenAI 官方端点为 true；确认支持该字段的其他兼容端点可显式开启。
func WithOpenAIPromptCacheKey(supported bool) OpenAIOption {
	return func(a *OpenAIAdapter) { a.caps.SupportsPromptCacheKey = supported }
}

// openAIOfficialBase 官方端点前缀，用于默认开启 SupportsPromptCacheKey。
const openAIOfficialBase = "https://api.openai.com"

// promptCacheKeyFromCtx 由会话标识派生 prompt_cache_key：sha256 前 16 hex，不含任何原文。
// ctx 无会话标识（后台调用等）时返回空串，调用方据此不发送该字段。
func promptCacheKeyFromCtx(ctx context.Context) string {
	sid, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	if sid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("polaris-prompt-cache-key:" + sid))
	return hex.EncodeToString(sum[:])[:16]
}

// applyPromptCacheFields 仅对声明支持的端点写入 prompt_cache_key / prompt_cache_retention，
// 避免给不认识该字段的兼容端点发送未知参数。retention 只在存在 key 时才有意义，一并省略。
func (a *OpenAIAdapter) applyPromptCacheFields(ctx context.Context, apiReq *OpenAIRequest) {
	if !a.caps.SupportsPromptCacheKey {
		return
	}
	key := promptCacheKeyFromCtx(ctx)
	if key == "" {
		return
	}
	apiReq.PromptCacheKey = key
	apiReq.PromptCacheRetention = a.cacheRetention
}

// NewOpenAIAdapter 初始化一个 OpenAI 适配器。
// baseURL 默认为 "https://api.openai.com/v1"（如果传入空串）。
// credPool 支持多 API Key 轮换（P1 2026-07-12）：单 key 场景用
// llmparent.NewCredentialPool(splitAPIKeys(key), llmparent.StrategyRoundRobin) 构造。
func NewOpenAIAdapter(baseURL, model string, credPool *llmparent.CredentialPool, client *http.Client, tbr *metrics.TokenBurnRate, opts ...OpenAIOption) *OpenAIAdapter {
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

	a := &OpenAIAdapter{
		model:    model,
		credPool: credPool,
		client:   c,
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			// gpt-4o / gpt-4o-mini 等均支持视觉输入；client.go parseImagePart 已实现
			// 此处声明后路由层才能在多模态请求时将 OpenAI 纳入候选
			SupportsVision:   true,
			MaxContextTokens: 128000,
			CostPer1KInput:   0.15,
			CostPer1KOutput:  0.60,
			// 仅官方端点认识 prompt_cache_key；Ollama/自建/其他兼容端点默认不发（见 WithOpenAIPromptCacheKey）。
			SupportsPromptCacheKey: strings.HasPrefix(baseURL, openAIOfficialBase),
		},
		tbr: tbr,
	}
	for _, o := range opts {
		o(a)
	}
	return a
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
		Messages:       msgs,
		Model:          options.Model,
		MaxTokens:      options.MaxTokens,
		Tools:          options.Tools,
		ToolChoice:     options.ToolChoice,
		ThinkingMode:   options.ThinkingMode,
		Temperature:    options.Temperature,
		ResponseFormat: options.ResponseFormat,
	}
	apiReq := translateRequest(req, a.caps.SupportsVision)
	a.applyPromptCacheFields(ctx, apiReq)
	apiReq.Model = resolveOpenAIModel(a.model)
	if req.Model != "" {
		apiReq.Model = resolveOpenAIModel(req.Model)
	}

	cred := a.credPool.Pick()
	if cred == nil {
		return nil, apperr.New(apperr.CodeResourceExhausted, "OpenAIAdapter.Infer: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()
	defer llmparent.ClearBytes(apiKey)

	resp, err := a.client.SendRequest(ctx, apiKey, apiReq)
	cred.RecordResult(err)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "OpenAIAdapter.Infer", err)
	}

	// Model 取响应回报的模型 ID（此前误填 resp.ID，即 chatcmpl-* 响应标识）。
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	out := &types.ProviderResponse{
		Model: model,
		Usage: resp.Usage.toUsage(),
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

	if out.Content == "" && len(out.ToolCalls) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "llm: empty response from provider")
	}

	return out, nil
}

func (a *OpenAIAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		//nolint:govet // cancel is intentionally passed to SafeGo
		ctx, cancel = context.WithTimeout(ctx, defaultStreamInferTimeout)
	}
	options := &types.InferOptions{}
	for _, opt := range opts {
		opt(options)
	}
	req := &types.InferRequest{
		Messages:       msgs,
		Model:          options.Model,
		MaxTokens:      options.MaxTokens,
		Tools:          options.Tools,
		ToolChoice:     options.ToolChoice,
		ThinkingMode:   options.ThinkingMode,
		Temperature:    options.Temperature,
		ResponseFormat: options.ResponseFormat,
	}
	apiReq := translateRequest(req, a.caps.SupportsVision)
	a.applyPromptCacheFields(ctx, apiReq)
	apiReq.Model = resolveOpenAIModel(a.model)
	if req.Model != "" {
		apiReq.Model = resolveOpenAIModel(req.Model)
	}

	cred := a.credPool.Pick()
	if cred == nil {
		if cancel != nil {
			cancel()
		}
		return nil, apperr.New(apperr.CodeResourceExhausted, "OpenAIAdapter.StreamInfer: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()
	defer llmparent.ClearBytes(apiKey)

	tok := llmparent.NewTiktokenTokenizer(a.model)
	rawCh, err := a.client.SendStreamRequest(ctx, cancel, apiKey, apiReq, tok.EstimateRequest(req))
	cred.RecordResult(err)
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
					// SendStreamRequest 仅在末事件（收到完整 usage 后）填充 InputTokens/
					// OutputTokens，中间 delta 事件该字段为 0，故此处不会重复计数。
					a.tbr.Add(int64(ev.Usage.InputTokens + ev.Usage.OutputTokens))
				}
			}
			if ev.Usage.CacheHitTokens > 0 || ev.Usage.InputTokens > 0 {
				metrics.RecordLLMCacheHit("openai", req.Model, ev.Usage.CacheHitTokens > 0)
			}
			select {
			case outCh <- ev:
			case <-ctx.Done():
				return
			}
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
