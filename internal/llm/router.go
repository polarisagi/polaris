package llm

import (
	"github.com/polarisagi/polaris/internal/observability/trace"

	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/polarisagi/polaris/internal/llm/modelregistry"
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
	modelRegistry *modelregistry.Registry
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

// recordModelCallResult 把一次 Provider 调用结果同步给 ModelVersionRegistry
// （2026-07-14 ADR-0051 关联接线：Registry.RecordCallResult 此前已完整实现连续
// 失败计数 + FindPredecessor 回退建议，但路由层从未持有 Registry 实例、从未
// 调用过它，数据一直是空的）。modelRegistry 为 nil（未注入）时整体是 no-op。
// shouldRollback=true 时目前只做可观测日志：路由的 Provider 选择由
// ProviderRegistry.best()/entry.recordOutcome 的健康度评分驱动，动态把某个
// entry 背后的具体 modelID 热替换为 rollbackToModelID 需要改造
// ProviderRegistry 条目结构本身，属于更大的设计变更，不在本次接线范围内；
// 先把追踪数据和建议接上，让 sysadmin/运维可观测到，后续如需自动执行回退
// 再单独设计执行路径。
func (ir *InferenceRouter) recordModelCallResult(ctx context.Context, providerName, modelID string, success bool) {
	if ir.modelRegistry == nil || modelID == "" {
		return
	}
	shouldRollback, rollbackTo, err := ir.modelRegistry.RecordCallResult(ctx, providerName, modelID, success)
	if err != nil {
		slog.Warn("inference_router: RecordCallResult failed", "provider", providerName, "model", modelID, "err", err)
		return
	}
	if shouldRollback {
		slog.Warn("inference_router: model consecutive failures reached rollback threshold",
			"provider", providerName, "model", modelID, "suggested_rollback_to", rollbackTo)
	}
}

func (ir *InferenceRouter) InjectOutboxWriter(w protocol.OutboxWriter) {
	ir.outboxWriter = w
}

// InjectModelRegistry 启动期后置注入 ModelVersionRegistry（modelReg 的构造依赖
// sb.Store.DB()，在 boot_memory.go 中晚于 router 本身构造完成，故提供 Inject*
// 形式而非要求 boot_substrate.go 在构造 router 时就持有它，与 InjectOutboxWriter
// 的既有模式一致）。
func (ir *InferenceRouter) InjectModelRegistry(reg *modelregistry.Registry) {
	ir.modelRegistry = reg
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
	// 2026-07-14（ADR-0051 关联接线）：改用 protocol.ApplyInferOptions 复用统一实现，
	// 消除与该函数重复的内联 for-range opt(options) 循环（此前 router.go 内两处、
	// protocol.ApplyInferOptions 一处，三份同构代码）。行为等价：
	// ApplyInferOptions 显式给 ThinkingMode 填充 types.ThinkingDisabled 默认值，
	// 而非零值 ""；两者在全部消费方（adapter/*.go）的判断条件
	// `req.ThinkingMode != "" && req.ThinkingMode != types.ThinkingDisabled` 下
	// 完全等价，不改变实际路由行为。
	appliedOpts := protocol.ApplyInferOptions(opts)
	options := &appliedOpts
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
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed).WithRetryAfter(30)
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
	ir.recordModelCallResult(ctx, entry.name, entry.provider.ModelID(), err == nil)
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
	// 2026-07-14（ADR-0051 关联接线）：改用 protocol.ApplyInferOptions 复用统一实现，
	// 消除与该函数重复的内联 for-range opt(options) 循环（此前 router.go 内两处、
	// protocol.ApplyInferOptions 一处，三份同构代码）。行为等价：
	// ApplyInferOptions 显式给 ThinkingMode 填充 types.ThinkingDisabled 默认值，
	// 而非零值 ""；两者在全部消费方（adapter/*.go）的判断条件
	// `req.ThinkingMode != "" && req.ThinkingMode != types.ThinkingDisabled` 下
	// 完全等价，不改变实际路由行为。
	appliedOpts := protocol.ApplyInferOptions(opts)
	options := &appliedOpts
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
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed).WithRetryAfter(30)
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
	ir.recordModelCallResult(ctx, entry.name, entry.provider.ModelID(), err == nil)
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

// StreamInferWithTarget 直接使用指定 Provider 发起推理，并复用本 Router 的 governance (AdmitLLM) 与 metrics，绕过 failover 机制。
func (ir *InferenceRouter) StreamInferWithTarget(ctx context.Context, p protocol.Provider, providerName string, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	appliedOpts := protocol.ApplyInferOptions(opts)
	req := &types.InferRequest{
		Messages:       msgs,
		Model:          appliedOpts.Model,
		MaxTokens:      appliedOpts.MaxTokens,
		Tools:          appliedOpts.Tools,
		ThinkingMode:   appliedOpts.ThinkingMode,
		Temperature:    appliedOpts.Temperature,
		ResponseFormat: appliedOpts.ResponseFormat,
		ThinkingBudget: appliedOpts.ThinkingBudget,
	}
	normalizeInferRequest(req)

	if err := ir.acquireLLMCapacity(ctx); err != nil {
		return nil, err
	}

	ch, err := p.StreamInfer(ctx, msgs, opts...)
	ir.recordModelCallResult(ctx, providerName, p.ModelID(), err == nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "StreamInferWithTarget", err)
	}
	return ir.wrapStreamChannel(ctx, ch, req, providerName), nil
}

// wrapStreamChannel / streamFailover 见 router_stream.go（R7 拆分）。
// Capabilities / Tokenizer / failover / findBestProviderLocked* / recordFailoverMetrics /
// ClearBytes / max64 / acquireLLMCapacity 见 router_failover.go（R7 拆分）。
