package llm

import (
	"context"
	"log/slog"

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
// 2026-07-08 移除原 providerName/model 形参 + 逐 token 用量累积统计（见
// local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md）：
// 该统计此前仅用于喂给已删除的 writeStreamEvent/eventWriter 死代码分支，
// 本身没有其它消费方，一并清理（2 处调用方已同步更新，见 router.go/router_failover.go）。
func (ir *InferenceRouter) wrapStreamChannel(ctx context.Context, ch <-chan types.StreamEvent) <-chan types.StreamEvent {
	out := make(chan types.StreamEvent)
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
				out <- ev
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
			return ir.wrapStreamChannel(ctx, ch), nil
		}

		skipped[chosen.name] = struct{}{}
		slog.Warn("inference_router: stream failover attempt failed, trying next",
			"provider", chosen.name, "err", err, "tried", len(skipped))
	}
}
