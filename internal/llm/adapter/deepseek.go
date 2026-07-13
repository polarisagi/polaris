package adapter

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"

	"context"
	"net/http"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// DeepSeekAdapter 实现 protocol.Provider，对接 DeepSeek 官方 API (兼容 OpenAI 格式)。
type DeepSeekAdapter struct {
	credPool     *llmparent.CredentialPool
	client       *OpenAICompatibleClient
	capabilities types.ProviderCapabilities
	modelID      string // 通过配置注入，默认 "deepseek-v4-flash"
	tbr          *metrics.TokenBurnRate
}

// NewDeepSeekAdapter 构造 DeepSeek 适配器。
// modelID 传 "" 时默认使用 "deepseek-v4-flash"（V4 Flash，低成本推理）；
// 传 "deepseek-v4-pro" 时启用 1M context 上限。
// credPool 支持多 API Key 轮换（P1 2026-07-12）：单 key 场景用
// llmparent.NewSingleCredentialPool(key) 构造。
func NewDeepSeekAdapter(credPool *llmparent.CredentialPool, httpClient *http.Client, modelID string, tbr *metrics.TokenBurnRate) *DeepSeekAdapter {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}

	maxCtx := 65536 // v4-flash 默认
	if modelID == "deepseek-v4-pro" || modelID == "deepseek-reasoner" {
		maxCtx = 1_000_000 // V4 Pro 支持 1M context
	}

	c := &OpenAICompatibleClient{
		BaseURL:    "https://api.deepseek.com/v1", // DeepSeek 兼容入口
		HTTPClient: httpClient,
	}

	return &DeepSeekAdapter{
		credPool: credPool,
		client:   c,
		modelID:  modelID,
		capabilities: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsThinking:  true,
			MaxContextTokens:  maxCtx,
			CostPer1KInput:    0.14, // 预估费率
			CostPer1KOutput:   0.28,
		},
		tbr: tbr,
	}
}

func (d *DeepSeekAdapter) ModelID() string {
	return d.modelID
}

func (d *DeepSeekAdapter) Capabilities() types.ProviderCapabilities {
	return d.capabilities
}

func (d *DeepSeekAdapter) Tokenizer() protocol.TokenizerAdapter {
	// DeepSeek 词表与 cl100k_base 高度兼容，用 tiktoken cl100k_base 估算误差 <5%
	return llmparent.NewTiktokenTokenizer("deepseek-v4")
}

// Infer 阻塞执行单次全量推理。
func (d *DeepSeekAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
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
		ThinkingMode:   options.ThinkingMode,
		Temperature:    options.Temperature,
		ResponseFormat: options.ResponseFormat,
	}
	apiReq := translateRequest(req, d.capabilities.SupportsVision)
	cred := d.credPool.Pick()
	if cred == nil {
		return nil, apperr.New(apperr.CodeResourceExhausted, "DeepSeekAdapter.Infer: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()
	defer llmparent.ClearBytes(apiKey)

	apiReq.Model = resolveDeepSeekModel(apiReq.Model)

	resp, err := d.client.SendRequest(ctx, apiKey, apiReq)
	cred.RecordResult(err)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "DeepSeekAdapter.Infer", err)
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
		if d.tbr != nil {
			d.tbr.Add(int64(out.Usage.InputTokens + out.Usage.OutputTokens))
		}
	}

	metrics.RecordLLMCacheHit("deepseek", req.Model, out.Usage.CacheHitTokens > 0)

	if len(resp.Choices) > 0 {
		contentStr, _ := resp.Choices[0].Message.Content.(string)
		out.Content = contentStr
		out.FinishReason = resp.Choices[0].FinishReason
		// thinking mode 下提取 reasoning_content（非 thinking 模式时为空字符串，安全）
		out.ReasoningContent = resp.Choices[0].Message.ReasoningContent
		// 提取 tool_calls（若有）
		if len(resp.Choices[0].Message.ToolCalls) > 0 {
			for _, tc := range resp.Choices[0].Message.ToolCalls {
				out.ToolCalls = append(out.ToolCalls, types.InferToolCall{
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: []byte(tc.Function.Arguments),
				})
			}
		}
	}

	return out, nil
}

// StreamInfer 执行流式推理并返回事件通道。
func (d *DeepSeekAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
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
		Messages:       msgs,
		Model:          options.Model,
		MaxTokens:      options.MaxTokens,
		Tools:          options.Tools,
		ThinkingMode:   options.ThinkingMode,
		Temperature:    options.Temperature,
		ResponseFormat: options.ResponseFormat,
	}
	apiReq := translateRequest(req, d.capabilities.SupportsVision)
	cred := d.credPool.Pick()
	if cred == nil {
		return nil, apperr.New(apperr.CodeResourceExhausted, "DeepSeekAdapter.StreamInfer: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()
	defer llmparent.ClearBytes(apiKey)

	apiReq.Model = resolveDeepSeekModel(apiReq.Model)

	tok := llmparent.NewTiktokenTokenizer("deepseek-v4")
	rawCh, err := d.client.SendStreamRequest(ctx, apiKey, apiReq, tok.EstimateRequest(req))
	// 建连/握手阶段的结果先行回报；建连成功后逐块解码期间的错误（rawCh 内部）
	// 属于流中断，现有设计未对单块错误做凭证级分类，维持原有粒度不扩大改动范围。
	cred.RecordResult(err)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "DeepSeekAdapter.StreamInfer", err)
	}

	outCh := make(chan types.StreamEvent, 100)
	// [SafeGo] 计费/缓存指标转发：畸形事件触发 panic 此前会直接崩进程。
	concurrent.SafeGo(ctx, "llm.adapter.deepseek_stream_relay", func(context.Context) {
		defer close(outCh)
		for ev := range rawCh {
			if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
				if d.tbr != nil {
					d.tbr.Add(int64(ev.Usage.InputTokens + ev.Usage.OutputTokens))
				}
			}
			if ev.Usage.CacheHitTokens > 0 || ev.Usage.InputTokens > 0 {
				metrics.RecordLLMCacheHit("deepseek", req.Model, ev.Usage.CacheHitTokens > 0)
			}
			outCh <- ev
		}
	})
	return outCh, nil
}

// resolveDeepSeekModel 负责将旧模型名称迁移到新模型名称（90天过渡期 fallback）
func resolveDeepSeekModel(model string) string {
	switch model {
	case "", "deepseek-chat":
		return "deepseek-v4-flash"
	case "deepseek-reasoner":
		return "deepseek-v4-pro"
	default:
		return model
	}
}
