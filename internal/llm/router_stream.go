package llm

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 流式路由：流通道封装 + 流式 Failover（R7 拆分自 router.go）。
// 结构体/构造/Infer/StreamInfer 见 router.go；
// Capabilities/Tokenizer/非流式 failover/负载均衡辅助 见 router_failover.go。
// ============================================================================

// wrapStreamChannel 封装流处理，以便在流结束或中断时正确释放 governor 许可证。
//
// 2026-07-12 P1 修复：重新引入 req/providerName 形参（2026-07-08 曾因唯一消费方
// eventWriter 死代码被清理而移除），本次是为了接入 StreamBudgetGuard +
// TokenBurnDetector（M01 §5.2-5.4，此前从未构造）——单流失控加速消耗 token 时
// 之前完全没有单流级别的硬阻断，系统级 TokenBurnRate gauge 只能事后观测，不能
// 提前掐断。req.MaxTokens 作为本次流的预算上限（<=0 视为无预算上限，只做加速度
// 检测）；burnDetector 用 5s 窗口检测 token 输出加速度异常（3 倍以上 → 硬阻断）。
func (ir *InferenceRouter) wrapStreamChannel(ctx context.Context, ch <-chan types.StreamEvent, req *types.InferRequest, providerName string) <-chan types.StreamEvent { //nolint:gocyclo
	out := make(chan types.StreamEvent)
	maxBufBytes := ir.registry.cfg.MaxStreamBufferKB * 1024
	if maxBufBytes <= 0 {
		maxBufBytes = 256 * 1024 // 与 TrackStreamCost/M1RouterThresholds 默认值一致的兜底
	}
	guard := NewStreamBudgetGuard(NewTokenBudget(req.MaxTokens), NewTokenBurnDetector(5000), maxBufBytes)
	accumulatedBytes := 0
	// [SafeGo] 全 Provider 流式路径的统一出口：任一 Provider 实现的畸形事件
	// 均经此处 relay，此前无 recover 会直接崩进程。
	concurrent.SafeGo(ctx, "llm.router.stream_channel_relay", func(ctx context.Context) {
		defer close(out)
		if ir.governor != nil {
			defer ir.governor.ReleaseLLM()
		}
		for {
			select {
			case <-ctx.Done():
				errStr := "context cancelled"
				if ctx.Err() != nil {
					errStr = ctx.Err().Error()
				}
				select {
				case out <- types.StreamEvent{
					Type:    types.StreamCancelled,
					Content: errStr,
				}:
				default:
				}
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if ev.Type == types.StreamTextDelta || ev.Type == types.StreamThinking {
					accumulatedBytes += len(ev.Content)
					tokens := ev.Usage.OutputTokens
					if tokens == 0 && ev.Content != "" {
						tokens = len(ev.Content) / 4 // SimpleTokenizer 同款粗估，无逐块 usage 时的兜底
					}
					guard.sessionBudget.Consume(tokens)
					if gErr := guard.GuardChunk(ctx, tokens); gErr != nil {
						slog.Warn("stream budget exceeded", "remaining", guard.sessionBudget.Remaining(), "window", guard.burnDetector.GetWindow())
						ir.abortStream(ctx, out, providerName, gErr)
						return
					}
					if accumulatedBytes > guard.GetMaxBufferSize() {
						slog.Warn("stream size exceeded", "limit", guard.GetMaxBufferSize())
						ir.abortStream(ctx, out, providerName, ErrResponseTooLarge)
						return
					}
					if szErr := TrackStreamCost(ctx, accumulatedBytes, providerName); szErr != nil {
						ir.abortStream(ctx, out, providerName, szErr)
						return
					}
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	})
	return out
}

// abortStream 因 StreamBudgetGuard/TrackStreamCost 硬阻断而中止流：向下游发一个
// StreamCancelled 事件，并记录一条可观测的 LLM 调用结果，复用既有
// trace.RecordLLMCall 管线（避免为此新增一套 Prometheus/OTel instrument）。
//
// 发送策略是 `out <- ev` 与 `ctx.Done()` 二选一，**不是**此前的
// `select + default` 尽力而为：
//   - out 是无缓冲 channel，下游在 `for ev := range ch` 的两次迭代之间并不
//     停在接收上。default 分支因此会以竞态概率把这条终止事件直接丢掉——
//     下游看到的是一条"正常结束"的流，无法区分"生成完了"和"被预算守卫掐断"，
//     既是 HE-1 可观测性缺口，也让调用方无从重试/提示用户。
//     （表现为 TestWrapStreamChannel_BudgetExhaustionAborts 间歇性失败。）
//   - 改为阻塞发送不引入新的死锁面：正常中继路径（wrapStreamChannel 尾部）
//     本就是同一个 `out <- ev` / `ctx.Done()` 二选一，一个"不再消费又不取消
//     ctx"的下游在那里就已经会把 relay goroutine 挂住。两处阻塞剖面一致。
func (ir *InferenceRouter) abortStream(ctx context.Context, out chan<- types.StreamEvent, providerName string, cause error) {
	slog.Warn("inference_router: stream aborted by budget guard", "provider", providerName, "err", cause)
	trace.RecordLLMCall(ctx, providerName, "", "stream_aborted:"+cause.Error(), 0, 0, 0, 0, 0)
	select {
	case out <- types.StreamEvent{Type: types.StreamCancelled, Content: cause.Error()}:
	case <-ctx.Done():
	}
}

// streamPoolFallback 流式路径的跨 Model Pool 级联降级（GD-13-005 流式对偶）。
//
// 与非流式 tryPoolFallback 同一策略，两点差异源于流式语义：
//  1. 降级提示不能塞进 resp.DegradedFromPool（流式没有单一 response 对象），
//     改为在流首插入一个 StreamSystemNotice 事件，由 SessionOrchestrator 直接
//     转成用户可见的系统提示；
//  2. StreamInfer 只在"建流"这一刻能判断成败，建流成功后的错误由
//     wrapStreamChannel 处理，不再回到本函数。
//
// 链尾同样追加一次不限 role 的全局兜底（空串 Pool），理由见 tryPoolFallback。
func (ir *InferenceRouter) streamPoolFallback(ctx context.Context, msgs []types.Message, opts []types.InferOption, req *types.InferRequest) (<-chan types.StreamEvent, error) {
	originalPool := req.ModelPool
	if originalPool == "" {
		return nil, apperr.Wrap(apperr.CodeResourceExhausted,
			"inference_router: stream all providers exhausted", protocol.ErrAllProvidersFailed)
	}
	fallbacks := append(append([]string{}, ir.poolFallbackChain[originalPool]...), "")
	for _, fallbackPool := range fallbacks {
		if ctx.Err() != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.streamPoolFallback: ctx cancelled", ctx.Err())
		}
		degradedReq := *req
		degradedReq.ModelPool = fallbackPool

		ir.registry.mu.RLock()
		entry := ir.findBestProviderLockedMultiSkip(&degradedReq, nil)
		ir.registry.mu.RUnlock()
		if entry == nil {
			continue
		}

		slog.Warn("llm_router: stream target pool exhausted, attempting cross-pool fallback",
			"original_pool", originalPool, "fallback_pool", fallbackPool, "provider", entry.name)

		ch, err := entry.provider.StreamInfer(ctx, msgs, opts...)
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
			slog.Warn("llm_router: stream fallback pool also failed, trying next",
				"fallback_pool", fallbackPool, "err", err)
			continue
		}

		slog.Info("llm_router: stream cross-pool degraded inference succeeded",
			"original_pool", originalPool, "actual_pool", fallbackPool)
		wrapped := ir.wrapStreamChannel(ctx, ch, &degradedReq, entry.name)
		return prependDegradeNotice(ctx, wrapped, originalPool), nil
	}
	return nil, apperr.Wrap(apperr.CodeResourceExhausted,
		"inference_router: stream all providers exhausted including fallback pools for "+originalPool,
		protocol.ErrAllProvidersFailed)
}

// prependDegradeNotice 在降级后的流最前面插入一个系统提示事件，其余事件原样转发。
func prependDegradeNotice(ctx context.Context, in <-chan types.StreamEvent, originalPool string) <-chan types.StreamEvent {
	out := make(chan types.StreamEvent)
	concurrent.SafeGo(ctx, "llm.router.stream_degrade_notice", func(ctx context.Context) {
		defer close(out)
		notice := types.StreamEvent{
			Type:    types.StreamSystemNotice,
			Content: "「" + originalPool + "」模型池当前不可用，已自动切换至备用模型继续为你解答。",
		}
		select {
		case out <- notice:
		case <-ctx.Done():
			return
		}
		for ev := range in {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	})
	return out
}

// streamFailover 流式路径次优选择。
func (ir *InferenceRouter) streamFailover(ctx context.Context, msgs []types.Message, opts []types.InferOption, req *types.InferRequest, firstSkip string) (<-chan types.StreamEvent, error) {
	skipped := map[string]struct{}{firstSkip: {}}
	for {
		if ctx.Err() != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.streamFailover: ctx cancelled", ctx.Err())
		}
		ir.registry.mu.RLock()
		chosen := ir.findBestProviderLockedMultiSkip(req, skipped)
		ir.registry.mu.RUnlock()

		if chosen == nil {
			// 当前 Pool 内已无可试 Provider：进入跨 Pool 级联降级（GD-13-005）。
			// 此前流式路径完全没有这一步——Task 07 只给非流式 Infer 加了降级，
			// 而交互式对话走的恰恰是 StreamInfer，等于该特性在主用户路径上不生效。
			if req.ModelPool != "" {
				return ir.streamPoolFallback(ctx, msgs, opts, req)
			}
			return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: stream all providers exhausted", protocol.ErrAllProvidersFailed)
		}

		ch, err := chosen.provider.StreamInfer(ctx, msgs, opts...)
		chosen.recordOutcome(err == nil, func() {
			ir.registry.mu.RLock()
			fn := ir.registry.onRecovery
			name := chosen.name
			ir.registry.mu.RUnlock()
			if fn != nil {
				fn(name)
			}
		})
		ir.recordModelCallResult(ctx, chosen.name, chosen.provider.ModelID(), err == nil)

		if err == nil {
			return ir.wrapStreamChannel(ctx, ch, req, chosen.name), nil
		}

		ce := ClassifyWithProvider(err, chosen.name)
		if !ce.Retryable && !ce.ShouldFallback && !ce.ShouldRotateCredential {
			slog.Warn("inference_router: non-retryable stream error during failover, aborting remaining attempts",
				"provider", chosen.name, "reason", ce.Reason, "err", err, "tried", len(skipped)+1)
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.streamFailover: non-retryable ("+string(ce.Reason)+")", err)
		}
		if ce.Retryable && ce.Reason == ReasonRateLimit {
			select {
			case <-ctx.Done():
				return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.streamFailover: ctx cancelled during backoff", ctx.Err())
			case <-time.After(DefaultBackoff().DelayWithState(len(skipped), nil)):
			}
		}

		skipped[chosen.name] = struct{}{}
		slog.Warn("inference_router: stream failover attempt failed, trying next",
			"provider", chosen.name, "err", err, "tried", len(skipped))
	}
}
