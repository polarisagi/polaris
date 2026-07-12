package llm

import (
	"context"
	"log/slog"

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
func (ir *InferenceRouter) wrapStreamChannel(ctx context.Context, ch <-chan types.StreamEvent, req *types.InferRequest, providerName string) <-chan types.StreamEvent {
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
						ir.abortStream(ctx, out, providerName, gErr)
						return
					}
					if szErr := TrackStreamCost(ctx, accumulatedBytes, providerName); szErr != nil {
						ir.abortStream(ctx, out, providerName, szErr)
						return
					}
				}
				out <- ev
			}
		}
	})
	return out
}

// abortStream 因 StreamBudgetGuard/TrackStreamCost 硬阻断而中止流：向下游发一个
// 尽力而为的 StreamCancelled 事件（同 ctx.Done() 分支的非阻塞发送策略，避免下游
// 已不再消费时永久阻塞 relay goroutine），并记录一条可观测的 LLM 调用结果，复用
// 既有 trace.RecordLLMCall 管线（避免为此新增一套 Prometheus/OTel instrument）。
func (ir *InferenceRouter) abortStream(ctx context.Context, out chan<- types.StreamEvent, providerName string, cause error) {
	slog.Warn("inference_router: stream aborted by budget guard", "provider", providerName, "err", cause)
	trace.RecordLLMCall(ctx, providerName, "", "stream_aborted:"+cause.Error(), 0, 0, 0, 0, 0)
	select {
	case out <- types.StreamEvent{Type: types.StreamCancelled, Content: cause.Error()}:
	default:
	}
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

		if err == nil {
			return ir.wrapStreamChannel(ctx, ch, req, chosen.name), nil
		}

		if ce := ClassifyWithProvider(err, chosen.name); !ce.Retryable && !ce.ShouldFallback {
			slog.Warn("inference_router: non-retryable stream error during failover, aborting remaining attempts",
				"provider", chosen.name, "reason", ce.Reason, "err", err, "tried", len(skipped)+1)
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.streamFailover: non-retryable ("+string(ce.Reason)+")", err)
		}

		skipped[chosen.name] = struct{}{}
		slog.Warn("inference_router: stream failover attempt failed, trying next",
			"provider", chosen.name, "err", err, "tried", len(skipped))
	}
}
