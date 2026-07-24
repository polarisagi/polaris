package adapter

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	llmparent "github.com/polarisagi/polaris/internal/llm"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// GoogleAgentPlatformAdapter 对接 Google Agent Platform (GEAP / Gemini API)。
// 认证方式: ?key=apiKey 查询参数（同 polaris-gateway google translator）。
// 端点路由:
//   - projectID 非空 → GEAP 企业端点 (aiplatform.googleapis.com)
//   - projectID 为空 → Gemini Developer API (generativelanguage.googleapis.com)
type GoogleAgentPlatformAdapter struct {
	model     string
	projectID string
	location  string
	credPool  *llmparent.CredentialPool
	client    *http.Client
	caps      types.ProviderCapabilities
	tbr       *metrics.TokenBurnRate
}

var _ protocol.Provider = (*GoogleAgentPlatformAdapter)(nil)

// credPool 支持多 API Key 轮换（P1 2026-07-12）：单 key 场景用
// llmparent.NewSingleCredentialPool(key) 构造。
func NewGoogleAgentPlatformAdapter(model, projectID, location string, credPool *llmparent.CredentialPool, client *http.Client, tbr *metrics.TokenBurnRate) *GoogleAgentPlatformAdapter {
	if client == nil {
		client = defaultHTTPClient()
	}
	return &GoogleAgentPlatformAdapter{
		model:     model,
		projectID: projectID,
		location:  location,
		credPool:  credPool,
		client:    client,
		caps: types.ProviderCapabilities{
			SupportsStreaming: true,
			SupportsTools:     true,
			SupportsVision:    true, // Gemini 全系支持图像输入
			SupportsVideo:     true, // Gemini 1.5+ 支持视频文件输入
			MaxContextTokens:  1000000,
			CostPer1KInput:    0.075,
			CostPer1KOutput:   0.30,
		},
		tbr: tbr,
	}
}

func (a *GoogleAgentPlatformAdapter) ModelID() string                          { return a.model }
func (a *GoogleAgentPlatformAdapter) Capabilities() types.ProviderCapabilities { return a.caps }
func (a *GoogleAgentPlatformAdapter) Tokenizer() protocol.TokenizerAdapter {
	return &llmparent.SimpleTokenizer{}
}

// buildEndpoint / geapHostAndLoc / appendKey / geminiTool 系列类型 / buildGeminiRequest /
// parseGoogleStream 见 google_request.go（R7 拆分）。

func (a *GoogleAgentPlatformAdapter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
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
	body, err := buildGeminiRequest(req)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "GoogleAgentPlatformAdapter.Infer", err)
	}
	cred := a.credPool.Pick()
	if cred == nil {
		return nil, apperr.New(apperr.CodeResourceExhausted, "GoogleAgentPlatformAdapter.Infer: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()
	defer llmparent.ClearBytes(apiKey)

	endpoint := appendKey(a.buildEndpoint(false), string(apiKey))
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "GoogleAgentPlatformAdapter.Infer", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		cred.RecordResult(err)
		return nil, apperr.Wrap(apperr.CodeInternal, "GoogleAgentPlatformAdapter.Infer", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		callErr := apperr.New(apperr.CodeInternal, fmt.Sprintf("google: HTTP %d: %s", httpResp.StatusCode, raw))
		cred.RecordResult(callErr)
		return nil, callErr
	}
	cred.RecordResult(nil)

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string              `json:"text"`
					FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "google: decode", err)
	}

	text, finishReason := "", ""

	if len(out.Candidates) > 0 {
		finishReason = out.Candidates[0].FinishReason
		for _, p := range out.Candidates[0].Content.Parts {
			if p.Text != "" {
				text += p.Text
			}

		}
	}

	// Gemini doesn't use standard ToolCall struct in InferResponse yet, wait, polaris protocol uses string / json for stream but for non-stream what does it use?
	// The problem is InferResponse only has Content string. Tool calls in non-stream need to be handled if types.InferResponse supports it.
	// Looking at adapter_anthropic.go, Infer() only returns Content string. Our Tool calls are handled primarily in StreamInfer.
	// Wait, types.InferResponse doesn't have ToolCalls natively?
	// Let's check types.InferResponse definition via a quick look.

	resp := &types.ProviderResponse{
		Content:      text,
		FinishReason: finishReason,
		Model:        a.model,
		Usage: types.Usage{
			InputTokens:    out.UsageMetadata.PromptTokenCount,
			OutputTokens:   out.UsageMetadata.CandidatesTokenCount,
			CacheHitTokens: out.UsageMetadata.CachedContentTokenCount,
		},
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		if a.tbr != nil {
			a.tbr.Add(int64(resp.Usage.InputTokens + resp.Usage.OutputTokens))
		}
	}

	metrics.RecordLLMCacheHit("google", req.Model, resp.Usage.CacheHitTokens > 0)

	return resp, nil
}
func (a *GoogleAgentPlatformAdapter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	var outerCancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		//nolint:govet // cancel is intentionally passed to SafeGo
		ctx, outerCancel = context.WithTimeout(ctx, defaultStreamInferTimeout)
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
	body, err := buildGeminiRequest(req)
	if err != nil {
		if outerCancel != nil {
			outerCancel()
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "GoogleAgentPlatformAdapter.StreamInfer", err)
	}
	cred := a.credPool.Pick()
	if cred == nil {
		if outerCancel != nil {
			outerCancel()
		}
		return nil, apperr.New(apperr.CodeResourceExhausted, "GoogleAgentPlatformAdapter.StreamInfer: no available credential (all keys cooling down)")
	}
	apiKey := cred.CredFn()()

	endpoint := appendKey(a.buildEndpoint(true), string(apiKey))
	llmparent.ClearBytes(apiKey)

	// 给单次推理加 120s 上限，防止 Google 连接 hang 住永不关闭导致前端卡死
	inferCtx, inferCancel := context.WithTimeout(ctx, 120*time.Second)

	httpReq, err := http.NewRequestWithContext(inferCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		inferCancel()
		if outerCancel != nil {
			outerCancel()
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "GoogleAgentPlatformAdapter.StreamInfer", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		cred.RecordResult(err)
		inferCancel()
		if outerCancel != nil {
			outerCancel()
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "GoogleAgentPlatformAdapter.StreamInfer", err)
	}
	if httpResp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		httpResp.Body.Close()
		inferCancel()
		if outerCancel != nil {
			outerCancel()
		}
		callErr := apperr.New(apperr.CodeInternal, fmt.Sprintf("google: HTTP %d: %s", httpResp.StatusCode, raw))
		cred.RecordResult(callErr)
		return nil, callErr
	}
	cred.RecordResult(nil)

	ch := make(chan types.StreamEvent, 64)
	// [SafeGo] SSE 解码：畸形响应触发 panic 此前会直接崩进程；defer 链在 panic
	// 展开时仍会执行（cancel/close(ch)/Body.Close 均不受影响）。
	concurrent.SafeGo(inferCtx, "llm.adapter.google_stream_decode", func(ctx context.Context) {
		defer inferCancel()
		if outerCancel != nil {
			defer outerCancel()
		}
		defer close(ch)
		defer httpResp.Body.Close()
		parseGoogleStream(ctx, httpResp.Body, ch, a.model, a.tbr)
	})
	return ch, nil
}
