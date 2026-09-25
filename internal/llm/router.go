package llm

import (
	"google.golang.org/protobuf/proto"

	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol/pb"

	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/llm/modelregistry"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// InferenceRouter 实现 protocol.Provider，对上层透明地完成多厂商路由。
// 架构文档: docs/arch/M01-Inference-Runtime.md §4
type InferenceRouter struct {
	registry          *ProviderRegistry
	outboxWriter      protocol.OutboxWriter
	governor          LLMGovernor
	semanticCache     *search.SemanticCache
	modelRegistry     *modelregistry.Registry
	poolFallbackChain map[string][]string // Pool 级联降级链（GD-13-005）
	// streamInterrupts 记录流式中断事件（inv_M1_04），nil 时不落 EventLog。
	// 注入点见 InjectStreamInterruptRecorder（router_stream.go）。
	streamInterrupts StreamInterruptRecorder
	eventLogger      protocol.EventLogger
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

func (ir *InferenceRouter) InjectEventLogger(logger protocol.EventLogger) {
	ir.eventLogger = logger
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
	// dialer 形参保留作装配契约：生产恒传 SafeDialer，nil 仅见于单测。
	// 2026-09-19（GR-2.2-006）：删除 rateTracker/client 两个孤儿字段——Router 自身从不发 HTTP
	// （出站全部由各 Adapter 经 llmadapter.SetDefaultHTTPClient 注入的 SafeHTTPClient 完成），
	// 此处构造的 RateLimitCapturingTransport 客户端全仓零读取，限速头从未被捕获。
	if dialer == nil {
		slog.Warn("llm.NewInferenceRouter: dialer is nil (expected only in tests)")
	}
	ir := &InferenceRouter{
		registry: reg,
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
	entry := ir.registry.peekBest(nil)
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

	cached, ckey, useCache := ir.resolveSemanticCache(ctx, options, msgs, req.MaxTokens)
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
func (ir *InferenceRouter) resolveSemanticCache(ctx context.Context, options *types.InferOptions, msgs []types.Message, maxTokens int) (cached *types.ProviderResponse, ckey search.CacheKey, useCache bool) {
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
	if respStr, hit := ir.semanticCache.Get(ctx, ckey); hit {
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

	if ir.eventLogger != nil {
		payload := &pb.LLMCallPayload{
			Model:          resp.Model,
			Provider:       entry.name,
			InputTokens:    int32(resp.Usage.InputTokens),
			OutputTokens:   int32(resp.Usage.OutputTokens),
			CacheHitTokens: int32(resp.Usage.CacheHitTokens),
			CostUsd:        costUSD,
			LatencyMs:      int64(ms),
			FinishReason:   resp.FinishReason,
			RouteTier:      entry.role,
		}
		b, err := proto.Marshal(payload)
		if err == nil {
			now := time.Now().UnixMicro()
			evID := util.GenerateHumanReadableID("evt", "llm call recorded")
			ev := &pb.Event{
				Id:             evID,
				Topic:          "llm.call.recorded",
				Actor:          "router",
				Type:           "llm_call",
				IdempotencyKey: string(types.BuildIdempotencyKey("llm", "call_recorded", evID, "record", 0)),
				OccurredAt:     now,
				CreatedAt:      now,
				Payload:        b,
			}
			// 不阻塞主路径
			if err := ir.eventLogger.AppendEvent(context.Background(), ev); err != nil {
				slog.Error("router: failed to write llm_call event", "err", err)
			}
		}
	}

	if useCache && len(resp.ToolCalls) == 0 {
		if cErr := ir.semanticCache.Put(ctx, ckey, resp.Content, resp.Model); cErr != nil {
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

	// 额度在选路之前获取：所有成功返回的流都经 wrapStreamChannel 在关闭时
	// ReleaseLLM，含下方目标 Pool 为空直接走 streamPoolFallback 的分支——此前
	// 该分支先于 acquire 返回，流关闭时"没借就还"，llmInFlight 逐轮变负，并发
	// 上限实际失效（FSM 回合走 general 池，而 DeepSeek 种子只有 default/reasoning）。
	if err := ir.acquireLLMCapacity(ctx); err != nil {
		return nil, err
	}
	// [2026-09-22 修复] 额度此前只在成功路径（wrapStreamChannel 内部的 deferred
	// ReleaseLLM，流真正关闭时才释放）归还；错误返回路径不释放，额度永久泄漏——
	// 连续几次失败调用（如 Provider 配置错误）即可耗尽默认上限。released 标记确保：
	// 成功路径把释放责任移交给 wrapStreamChannel，其余路径在此兜底释放。
	released := false
	if ir.governor != nil {
		defer func() {
			if !released {
				ir.governor.ReleaseLLM()
			}
		}()
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
		// 与 Infer 同构：目标 Pool 内无可用 Provider 时不直接拒绝，先走
		// 流式跨 Pool 降级链（GD-13-005）。
		if req.ModelPool != "" {
			fch, ferr := ir.streamPoolFallback(ctx, msgs, opts, req)
			released = ferr == nil
			return fch, ferr
		}
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers failed", protocol.ErrAllProvidersFailed).WithRetryAfter(30)
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

		fch, ferr := ir.streamFailover(ctx, msgs, opts, req, entry.name)
		if ferr == nil {
			released = true
		}
		return fch, ferr
	}

	released = true
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
		// 与 StreamInfer 同一泄漏：额度只由 wrapStreamChannel 在流关闭时归还，
		// 错误路径须在此释放。
		if ir.governor != nil {
			ir.governor.ReleaseLLM()
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "StreamInferWithTarget", err)
	}
	return ir.wrapStreamChannel(ctx, ch, req, providerName), nil
}

// wrapStreamChannel / streamFailover 见 router_stream.go（R7 拆分）。
// Capabilities / Tokenizer / failover / findBestProviderLocked* / recordFailoverMetrics /
// ClearBytes / max64 / acquireLLMCapacity 见 router_failover.go（R7 拆分）。
