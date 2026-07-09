package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// AnthropicAdapter 实现 protocol.Provider，对接 Anthropic Messages API。
//
// 请求体构建（buildAnthropicRequest/prompt caching）、SSE 流解析
// （parseAnthropicStream）、模型名解析与 keyInjectRT 见 anthropic_request.go（R7 拆分）。
type AnthropicAdapter struct {
	model               string
	credentialFn        func() []byte
	client              *http.Client
	caps                types.ProviderCapabilities
	enablePromptCaching bool   // 注入 cache_control 标记以激活 prompt caching
	baseURL             string // 空值 → "https://api.anthropic.com"（测试可覆盖）
	tbr                 *metrics.TokenBurnRate
}

var _ protocol.Provider = (*AnthropicAdapter)(nil)

// AnthropicOption 适配器选项函数。
type AnthropicOption func(*AnthropicAdapter)

// WithAnthropicPromptCaching 开启 Anthropic prompt caching。
// 向 system prompt 和最后一个 tool 注入 cache_control:{type:"ephemeral"}，
// 命中缓存时 cache_read_input_tokens 费率约为正常输入的 1/10。
func WithAnthropicPromptCaching() AnthropicOption {
	return func(a *AnthropicAdapter) {
		a.enablePromptCaching = true
		a.caps.CostPer1KCacheHit = 0.30 // Anthropic cache read: $0.30/1M tokens
	}
}

// NewAnthropicAdapter 构造 Anthropic 适配器。
func NewAnthropicAdapter(model string, credFn func() []byte, client *http.Client, tbr *metrics.TokenBurnRate, opts ...AnthropicOption) *AnthropicAdapter {
	if client == nil {
		client = defaultHTTPClient()
	}

	// Wrap transport for key injection
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	customClient := *client // shallow copy
	customClient.Transport = keyInjectRT{inner: transport, keyFn: credFn}

	a := &AnthropicAdapter{
		model:        model,
		credentialFn: credFn,
		client:       &customClient,
		tbr:          tbr,
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsThinking:  true,
			SupportsVision:    true, // Claude 3+ 全系支持图像输入
			MaxContextTokens:  200000,
			CostPer1KInput:    3.0,
			CostPer1KOutput:   15.0,
		},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// messagesURL 返回 Messages API 端点（测试可通过 baseURL 覆盖）。
func (a *AnthropicAdapter) messagesURL() string {
	base := a.baseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return base + "/v1/messages"
}

func (a *AnthropicAdapter) ModelID() string {
	return a.model
}

func (a *AnthropicAdapter) Capabilities() types.ProviderCapabilities {
	return a.caps
}

func (a *AnthropicAdapter) Tokenizer() protocol.TokenizerAdapter {
	return &llmparent.SimpleTokenizer{}
}

func (a *AnthropicAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
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
	body, err := a.buildAnthropicRequest(req, false)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "AnthropicAdapter.Infer", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.messagesURL(), bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "AnthropicAdapter.Infer", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if req.ThinkingMode != "" && req.ThinkingMode != types.ThinkingDisabled {
		httpReq.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
	}

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "AnthropicAdapter.Infer", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("anthropic: HTTP %d: %s", httpResp.StatusCode, raw))
	}

	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Model      string `json:"model"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "anthropic: decode", err)
	}

	textBuilder := new(strings.Builder)
	var toolCalls []types.InferToolCall
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			textBuilder.WriteString(c.Text)
		case "tool_use":
			input := []byte(c.Input)
			if len(input) == 0 {
				input = []byte("{}")
			}
			toolCalls = append(toolCalls, types.InferToolCall{
				ID:    c.ID,
				Name:  c.Name,
				Input: input,
			})
		}
	}
	resp := &types.ProviderResponse{
		Content:      textBuilder.String(),
		ToolCalls:    toolCalls,
		FinishReason: out.StopReason,
		Model:        out.Model,
		Usage: types.Usage{
			InputTokens:         out.Usage.InputTokens,
			OutputTokens:        out.Usage.OutputTokens,
			CacheHitTokens:      out.Usage.CacheReadInputTokens,
			CacheCreationTokens: out.Usage.CacheCreationInputTokens,
		},
	}

	// 扣除 token（EMA + 总量计算）
	if a.tbr != nil {
		a.tbr.Add(int64(resp.Usage.InputTokens + resp.Usage.OutputTokens))
	}

	hit := resp.Usage.CacheHitTokens > 0
	metrics.RecordLLMCacheHit("anthropic", req.Model, hit)

	return resp, nil
}

func (a *AnthropicAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
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
	body, err := a.buildAnthropicRequest(req, true)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "AnthropicAdapter.StreamInfer", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.messagesURL(), bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "AnthropicAdapter.StreamInfer", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if req.ThinkingMode != "" && req.ThinkingMode != types.ThinkingDisabled {
		httpReq.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
	}

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "AnthropicAdapter.StreamInfer", err)
	}

	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		httpResp.Body.Close()
		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("anthropic: HTTP %d: %s", httpResp.StatusCode, raw))
	}

	ch := make(chan types.StreamEvent, 64)
	// [SafeGo] SSE 解码：畸形响应触发 panic 此前会直接崩进程。
	concurrent.SafeGo(ctx, "llm.adapter.anthropic_stream_decode", func(ctx context.Context) {
		defer close(ch)
		defer httpResp.Body.Close()
		a.parseAnthropicStream(ctx, req.Model, httpResp.Body, ch)
	})
	return ch, nil
}
