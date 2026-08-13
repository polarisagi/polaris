package llm

import (
	"github.com/polarisagi/polaris/internal/observability/trace"

	"context"
	"log/slog"
	"net/http"
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
	registry          *ProviderRegistry
	rateTracker       *RateLimitTracker
	client            *http.Client
	outboxWriter      protocol.OutboxWriter
	governor          LLMGovernor
	semanticCache     *search.SemanticCache
	modelRegistry     *modelregistry.Registry
	poolFallbackChain map[string][]string // Pool 级联降级链（GD-13-005）
	// streamInterrupts 记录流式中断事件（inv_M1_04），nil 时不落 EventLog。
	// 注入点见 InjectStreamInterruptRecorder（router_stream.go）。
	streamInterrupts StreamInterruptRecorder
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

// 目标 Model Pool 的注入选项是 types.WithModelPool（与 types.WithModel 同处
// pkg/types，供 internal/agent 等上层调用方使用，避免它们为一个纯粹的
// options setter 反向依赖 internal/llm）。
//
// 注：不提供 WithPoolFallbackChain 之类的运行期覆盖选项——降级链是编译期常量
// （见下方 NewInferenceRouter）。此前存在的该 RouterOption 无任何生产调用点，
// 按 ADR-0062 deadcode 纪律（无 WIRE 决议 → 删除）移除；若将来确需配置化，
// 需先补 ADR 与 configs 键位。

// recordModelCallResult 把一次 Provider 调用结果同步给 ModelVersionRegistry
// （2026-07-14 ADR-0062 关联接线：Registry.RecordCallResult 此前已完整实现连续
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
	} else {
		// [2026-08-02 S-03 复核] dialer==nil 时 transport.DialContext 保持零值，
		// 退化为标准库默认拨号，绕过 SafeDialer 的 SSRF 防护（出站 LLM API 调用
		// 不再受 EgressAllowedDomains/内网地址拦截约束）。核实生产唯一装配点
		// cmd/polaris/boot_substrate.go:627 的 dialer 恒来自
		// network.NewSafeDialer(...)（该构造函数不存在返回 nil 的分支），
		// 故此分支当前生产不可达，nil 仅用于单测（httptest 本地服务器需绕过
		// SafeDialer 才能连通 127.0.0.1）。此处补一条日志而非改为 fail-closed
		// panic：既能在未来若真的因误改装配代码而意外触发时被立刻观测到
		// （HE-1），又不破坏现有依赖 nil-dialer 直连本地测试服务器的测试用例。
		slog.Warn("llm.NewInferenceRouter: dialer is nil, SafeDialer SSRF protection is bypassed for this router instance (expected only in tests)")
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
		// poolFallbackChain 定义当目标 Model Pool 所有 Provider 耗尽时的级联降级顺序（GD-13-005）。
		// 可通过 RouterOption 覆盖，当前默认值适配 reasoning/general/default/budget 四档分层。
		poolFallbackChain: map[string][]string{
			"reasoning": {"general", "default", "budget"},
			"general":   {"default", "budget"},
			"default":   {"budget"},
			"budget":    {},
		},
	}
	for _, opt := range opts {
		opt(ir)
	}
	reg.InjectRecoveryHandler(func(providerName string) {

		if ir.outboxWriter == nil {
			return
		}
		ev, evErr := protocol.NewOutboxEvent(protocol.TopicProviderRecovered, "provider_recovery", map[string]string{
			"event_type":    "m4_provider_recovery",
			"provider_name": providerName,
		}, string(types.BuildIdempotencyKey(protocol.TopicProviderRecovered, "provider", providerName, "recovery",
			int(time.Now().Unix()))))
		if evErr != nil {
			// 构造失败会返回零值 OutboxEntry（无 target_engine/payload），
			// 写出去只会污染 outbox；直接丢弃并告警。
			slog.Error("llm_router: build provider_recovery outbox event failed",
				"provider", providerName, "err", evErr)
			return
		}
		// 恢复通知丢失 = M4 侧永远收不到"该 Provider 已恢复"，会一直按熔断态
		// 绕开它直到下一次探活；必须告警而非静默（HE-1）。
		if err := ir.outboxWriter.Write(context.Background(), ev); err != nil {
			slog.Error("llm_router: provider recovery outbox write failed, downstream may keep provider circuit-open",
				"provider", providerName, "err", err)
		}
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
	// 2026-07-14（ADR-0062 关联接线）：改用 protocol.ApplyInferOptions 复用统一实现，
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
		ModelPool:      options.ModelPool,
	}

	normalizeInferRequest(req)

	cached, ckey, useCache := ir.resolveSemanticCache(options, msgs, req.MaxTokens)
	if cached != nil {
		return cached, nil
	}

	// ModelPool 非空时，初始选择也应严格按 role 过滤（GD-13-005）；
	// ModelPool 为空时使用全局 best()。
	var entry *providerEntry
	if req.ModelPool != "" {
		ir.registry.mu.RLock()
		entry = ir.findBestProviderLockedMultiSkip(req, nil)
		ir.registry.mu.RUnlock()
	} else {
		entry = ir.registry.best(req)
	}
	if entry == nil {
		// 指定了 Pool 却在该 Pool 内无可用 Provider：这与"该 Pool 的 Provider
		// 全部调用失败"是同一种状况，必须同样走跨 Pool 降级链（GD-13-005），
		// 而不是在这里直接拒绝——否则只要目标 Pool 一个 Provider 都没注册，
		// 降级机制就完全不会被触发（这正是本特性此前在生产中不可达的原因之一）。
		if req.ModelPool != "" {
			return ir.tryPoolFallback(ctx, msgs, opts, req)
		}
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed).WithRetryAfter(30)
	}

	if err := ir.acquireLLMCapacity(ctx); err != nil {
		return nil, err
	}
	if ir.governor != nil {
		defer ir.governor.ReleaseLLM()
	}

	start := time.Now()

	var err error
	defer func() {
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
	}()

	resp, inferErr := entry.provider.Infer(ctx, msgs, opts...)
	err = inferErr

	if err != nil {
		return ir.handleInferError(ctx, err, entry, msgs, opts, req)
	}

	ir.recordInferSuccess(ctx, entry, resp, float64(time.Since(start).Milliseconds()), useCache, ckey)
	return resp, nil
}

// resolveSemanticCache 检查语义缓存命中；未命中时返回可用于后续 Put 回写的 CacheKey
// 及 useCache 标记（从 Infer 拆出，gocyclo 治理，行为不变）。
func (ir *InferenceRouter) resolveSemanticCache(options *types.InferOptions, msgs []types.Message, maxTokens int) (cached *types.ProviderResponse, ckey search.CacheKey, useCache bool) {
	if ir.semanticCache == nil || options.CacheHints == nil {
		return nil, search.CacheKey{}, false
	}
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
				CacheHitTokens: maxTokens, // Approximation as we don't have exact token count here
			},
			Model:        "semantic_cache",
			FinishReason: "stop",
		}, ckey, true
	}
	return nil, ckey, true
}

// handleInferError 处理 provider.Infer 失败：ctx 已取消时直接透传；ErrorClassifier
// 判定不可重试/不应 failover 时直接失败；否则触发 failover 到次优 provider
// （从 Infer 拆出，gocyclo 治理，行为不变）。
func (ir *InferenceRouter) handleInferError(ctx context.Context, err error, entry *providerEntry, msgs []types.Message, opts []types.InferOption, req *types.InferRequest) (*types.ProviderResponse, error) {
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

// recordInferSuccess 记录成功调用的延迟/成本 trace，并在启用语义缓存时回写命中结果
// （从 Infer 拆出，gocyclo 治理，行为不变）。
func (ir *InferenceRouter) recordInferSuccess(ctx context.Context, entry *providerEntry, resp *types.ProviderResponse, ms float64, useCache bool, ckey search.CacheKey) {
	if resp == nil {
		return
	}
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
	// 不存在观测缺口。ADR-0025 §H 曾计划将此处的裸 goroutine 迁移到 SafeGo 并
	// 改经 event_buffer.go 批处理，但该 EventWriteBuffer 已确认零接线并删除，
	// 原计划的落地目标已不存在，遂一并清理。

	if useCache && len(resp.ToolCalls) == 0 {
		if cErr := ir.semanticCache.Put(ckey, resp.Content, resp.Model); cErr != nil {
			slog.WarnContext(ctx, "router: semantic cache put failed", "key", ckey, "err", cErr)
		}
	}
}

// StreamInfer 路由流式请求，内嵌延迟记录与 Failover。
func (ir *InferenceRouter) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	// 2026-07-14（ADR-0062 关联接线）：改用 protocol.ApplyInferOptions 复用统一实现，
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
		ModelPool:      options.ModelPool,
	}

	normalizeInferRequest(req)
	// ModelPool 非空时，初始选择也应严格按 role 过滤（GD-13-005）；
	// ModelPool 为空时使用全局 best()。
	var entry *providerEntry
	if req.ModelPool != "" {
		ir.registry.mu.RLock()
		entry = ir.findBestProviderLockedMultiSkip(req, nil)
		ir.registry.mu.RUnlock()
	} else {
		entry = ir.registry.best(req)
	}
	if entry == nil {
		// 与 Infer 同构：目标 Pool 内无可用 Provider 时不直接拒绝，先走
		// 流式跨 Pool 降级链（GD-13-005）。
		if req.ModelPool != "" {
			return ir.streamPoolFallback(ctx, msgs, opts, req)
		}
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed).WithRetryAfter(30)
	}

	if err := ir.acquireLLMCapacity(ctx); err != nil {
		return nil, err
	}

	start := time.Now()

	var err error
	defer func() {
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
	}()

	ch, streamErr := entry.provider.StreamInfer(ctx, msgs, opts...)
	err = streamErr
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
