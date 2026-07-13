package llm

import (
	"github.com/polarisagi/polaris/internal/observability/trace"

	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// InferenceRouter 实现 protocol.Provider，对上层透明地完成多厂商路由。
// 架构文档: docs/arch/M01-Inference-Runtime.md §4
type InferenceRouter struct {
	registry      *ProviderRegistry
	rateTracker   *RateLimitTracker
	client        *http.Client
	outboxWriter  protocol.OutboxWriter
	governor      LLMGovernor
	semanticCache *search.SemanticCache
}

// LLMGovernor 用于限流 LLM 请求 (P0-3)
type LLMGovernor interface {
	AdmitLLM(priority int) (bool, int)
	WaitForLLMCapacity(ctx context.Context) error
	ReleaseLLM()
}

type RouterOption func(*InferenceRouter)

func WithGovernor(gov LLMGovernor) RouterOption {
	return func(ir *InferenceRouter) {
		ir.governor = gov
	}
}

func WithSemanticCache(cache *search.SemanticCache) RouterOption {
	return func(ir *InferenceRouter) {
		ir.semanticCache = cache
	}
}

func (ir *InferenceRouter) InjectOutboxWriter(w protocol.OutboxWriter) {
	ir.outboxWriter = w
}

var _ protocol.Provider = (*InferenceRouter)(nil)

func NewInferenceRouter(reg *ProviderRegistry, dialer protocol.SafeDialer, opts ...RouterOption) *InferenceRouter {
	transport := &http.Transport{}
	if dialer != nil {
		transport.DialContext = dialer.DialContext
	}
	tracker := NewRateLimitTracker()
	ir := &InferenceRouter{
		registry:    reg,
		rateTracker: tracker,
		client: &http.Client{
			Transport: &RateLimitCapturingTransport{
				Inner:   transport,
				Tracker: tracker,
			},
			Timeout: 120 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(ir)
	}
	reg.InjectRecoveryHandler(func(providerName string) {

		if ir.outboxWriter == nil {
			return
		}
		ev, _ := protocol.NewOutboxEvent(protocol.TopicProviderRecovered, "provider_recovery", map[string]string{
			"event_type":    "m4_provider_recovery",
			"provider_name": providerName,
		}, "recovery:"+providerName+":"+strconv.FormatInt(time.Now().Unix(), 10))
		_ = ir.outboxWriter.Write(context.Background(), ev)
	})
	return ir
}

func (ir *InferenceRouter) ModelID() string {
	entry := ir.registry.best(nil)
	if entry == nil || entry.provider == nil {
		return "unknown"
	}
	return entry.provider.ModelID()
}

// Infer 路由单次请求到最优 Provider，失败时 failover 至次优。
func (ir *InferenceRouter) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
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
		ThinkingBudget: options.ThinkingBudget,
	}

	normalizeInferRequest(req)

	var ckey search.CacheKey
	var useCache bool
	if ir.semanticCache != nil && options.CacheHints != nil {
		msgStrs := make([]string, 0, len(msgs))
		for _, m := range msgs {
			msgStrs = append(msgStrs, m.Role+":"+m.Content)
		}
		ckey = search.CacheKey{
			ContextHintFingerprint: options.CacheHints.ContextHintFingerprint,
			ActiveControlLabels:    options.CacheHints.ActiveControlLabels,
			TaskType:               options.CacheHints.TaskType,
			Messages:               msgStrs,
		}
		if respStr, hit := ir.semanticCache.Get(ckey); hit {
			return &types.ProviderResponse{
				Content: respStr,
				Usage: types.Usage{
					CacheHitTokens: req.MaxTokens, // Approximation as we don't have exact token count here
				},
				Model:        "semantic_cache",
				FinishReason: "stop",
			}, nil
		}
		useCache = true
	}

	entry := ir.registry.best(req)
	if entry == nil {
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed)
	}

	if err := ir.acquireLLMCapacity(ctx); err != nil {
		return nil, err
	}
	if ir.governor != nil {
		defer ir.governor.ReleaseLLM()
	}

	start := time.Now()
	resp, err := entry.provider.Infer(ctx, msgs, opts...)
	ms := float64(time.Since(start).Milliseconds())
	entry.recordLatency(ms)
	entry.recordOutcome(err == nil, func() {
		ir.registry.mu.RLock()
		fn := ir.registry.onRecovery
		name := entry.name
		ir.registry.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})
	if err != nil {
		if ctx.Err() != nil {

			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.Infer", err)
		}

		// ErrorClassifier 接入（P1 2026-07-12）：此前任何错误一律 failover 到下一个
		// provider，包括请求格式错误/永久认证失效/策略拦截这类换 provider 也无法
		// 恢复的错误——既浪费时延，也可能把同一个畸形请求打到每一家 vendor。
		// Retryable=false 且 ShouldFallback=false 是 Classify() 对这类错误的明确信号。
		if ce := ClassifyWithProvider(err, entry.name); !ce.Retryable && !ce.ShouldFallback {
			slog.Warn("inference_router: non-retryable error, skip failover",
				"provider", entry.name, "reason", ce.Reason, "err", err)
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.Infer: non-retryable ("+string(ce.Reason)+")", err)
		}

		return ir.failover(ctx, msgs, opts, req, entry.name)
	}

	if resp != nil {
		caps := entry.provider.Capabilities()
		costUSD := float64(resp.Usage.InputTokens)*caps.CostPer1KInput/1000.0 +
			float64(resp.Usage.OutputTokens)*caps.CostPer1KOutput/1000.0 +
			float64(resp.Usage.CacheHitTokens)*caps.CostPer1KCacheHit/1000.0
		trace.RecordLLMCall(ctx,
			entry.name, resp.Model, "success", ms,
			resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheHitTokens,
			costUSD,
		)
		// 2026-07-08 移除 eventWriter 写事件分支（复核
		// code-quality-remediation-verification-20260707.md Phase 1.3 遗留项，
		// 详见 local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md）：
		// protocol.EventWriter 全仓库零实现，WithEventWriter 注入方法此前已被删除
		// 导致 eventWriter 恒为 nil、这段代码永久不可达；LLM 调用观测已由上面的
		// trace.RecordLLMCall（→ Prometheus/OTel InstrLLMCallsTotal 等）完整覆盖，
		// 不存在观测缺口。ADR-0029 §H 曾计划将此处的裸 goroutine 迁移到 SafeGo 并
		// 改经 event_buffer.go 批处理，但该 EventWriteBuffer 已确认零接线并删除，
		// 原计划的落地目标已不存在，遂一并清理。

		if useCache && len(resp.ToolCalls) == 0 {
			_ = ir.semanticCache.Put(ckey, resp.Content, resp.Model)
		}
	}
	return resp, nil
}

// StreamInfer 路由流式请求，内嵌延迟记录与 Failover。
func (ir *InferenceRouter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
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
		ThinkingBudget: options.ThinkingBudget,
	}

	normalizeInferRequest(req)
	entry := ir.registry.best(req)
	if entry == nil {
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed)
	}

	if err := ir.acquireLLMCapacity(ctx); err != nil {
		return nil, err
	}

	start := time.Now()
	ch, err := entry.provider.StreamInfer(ctx, msgs, opts...)
	entry.recordLatency(float64(time.Since(start).Milliseconds()))
	entry.recordOutcome(err == nil, func() {
		ir.registry.mu.RLock()
		fn := ir.registry.onRecovery
		name := entry.name
		ir.registry.mu.RUnlock()
		if fn != nil {
			fn(name)
		}
	})
	if err != nil {
		if ctx.Err() != nil {

			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.StreamInfer", err)
		}

		if ce := ClassifyWithProvider(err, entry.name); !ce.Retryable && !ce.ShouldFallback {
			slog.Warn("inference_router: non-retryable stream error, skip failover",
				"provider", entry.name, "reason", ce.Reason, "err", err)
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.StreamInfer: non-retryable ("+string(ce.Reason)+")", err)
		}

		return ir.streamFailover(ctx, msgs, opts, req, entry.name)
	}

	return ir.wrapStreamChannel(ctx, ch, req, entry.name), nil
}

// wrapStreamChannel / streamFailover 见 router_stream.go（R7 拆分）。
// Capabilities / Tokenizer / failover / findBestProviderLocked* / recordFailoverMetrics /
// ClearBytes / max64 / acquireLLMCapacity 见 router_failover.go（R7 拆分）。
