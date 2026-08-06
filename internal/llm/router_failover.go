package llm

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/observability/trace"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// Capabilities/Tokenizer + 非流式 Failover + Provider 负载均衡辅助（R7 拆分自 router.go）。
// 结构体/构造/Infer/StreamInfer 见 router.go；流式路由见 router_stream.go。
// ============================================================================

func (ir *InferenceRouter) Capabilities() types.ProviderCapabilities {

	caps := types.ProviderCapabilities{}
	ir.registry.mu.RLock()
	defer ir.registry.mu.RUnlock()
	for _, e := range ir.registry.entries {
		c := e.provider.Capabilities()
		if c.SupportsStreaming {
			caps.SupportsStreaming = true
		}
		if c.SupportsTools {
			caps.SupportsTools = true
		}
		if c.SupportsVision {
			caps.SupportsVision = true
		}
		if c.SupportsVideo {
			caps.SupportsVideo = true
		}
		if c.SupportsTTS {
			caps.SupportsTTS = true
		}
		if c.MaxContextTokens > caps.MaxContextTokens {
			caps.MaxContextTokens = c.MaxContextTokens
		}
	}
	return caps
}

func (ir *InferenceRouter) Tokenizer() protocol.TokenizerAdapter {
	entry := ir.registry.best(nil)
	if entry == nil {
		return &SimpleTokenizer{}
	}
	return entry.provider.Tokenizer()
}

func (ir *InferenceRouter) failover(ctx context.Context, msgs []types.Message, opts []types.InferOption, req *types.InferRequest, firstSkip string) (*types.ProviderResponse, error) {
	skipped := map[string]struct{}{firstSkip: {}}
	for {
		if ctx.Err() != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.failover: ctx cancelled", ctx.Err())
		}
		ir.registry.mu.RLock()
		chosen := ir.findBestProviderLockedMultiSkip(req, skipped)
		ir.registry.mu.RUnlock()

		if chosen == nil {
			// 当前 Pool 所有 Provider 耗尽，尝试跨 Pool 降级（GD-13-005）
			return ir.tryPoolFallback(ctx, msgs, opts, req)
		}
		start := time.Now()
		resp, err := chosen.provider.Infer(ctx, msgs, opts...)
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
		if err == nil && resp != nil {
			ir.recordFailoverMetrics(ctx, chosen, resp, start)
			return resp, nil
		}
		ce := ClassifyWithProvider(err, chosen.name)
		if !ce.Retryable && !ce.ShouldFallback && !ce.ShouldRotateCredential {
			slog.Warn("inference_router: non-retryable error during failover, aborting remaining attempts",
				"provider", chosen.name, "reason", ce.Reason, "err", err, "tried", len(skipped)+1)
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.failover: non-retryable ("+string(ce.Reason)+")", err)
		}
		if ce.Retryable && ce.Reason == ReasonRateLimit {
			select {
			case <-ctx.Done():
				return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.failover: ctx cancelled during backoff", ctx.Err())
			case <-time.After(DefaultBackoff().DelayWithState(len(skipped), nil)):
			}
		}
		skipped[chosen.name] = struct{}{}
		slog.Warn("inference_router: failover attempt failed, trying next",
			"provider", chosen.name, "err", err, "tried", len(skipped))
	}
}

// tryPoolFallback 当目标 Pool 所有 Provider 耗尽时，按 poolFallbackChain 尝试降级（GD-13-005）。
// 降级成功时在 resp.DegradedFromPool 中记录原始 Pool 名，供上层感知并通知用户。
func (ir *InferenceRouter) tryPoolFallback(ctx context.Context, msgs []types.Message, opts []types.InferOption, req *types.InferRequest) (*types.ProviderResponse, error) {
	originalPool := req.ModelPool
	if originalPool == "" {
		// 未指定 Pool 时全局耗尽，直接返回错误
		return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers exhausted", protocol.ErrAllProvidersFailed)
	}
	// 降级链末尾追加一次"不限 role 的全局兜底"（空串 Pool）：
	// findBestProviderLockedMultiSkip 对空 ModelPool 不做 role 过滤，等价于
	// 降级前的 best() 行为。这一档不可省——自托管最常见形态是只注册了一个
	// role 为空串的本地 Provider（boot_substrate.go reg.Register），若把 Pool
	// 当硬约束，指定 "reasoning" 会在健康 Provider 就在眼前时直接拒绝服务，
	// 比 GD-13-005 原本要修的"单池耗尽即断链"更糟。
	fallbacks := append(append([]string{}, ir.poolFallbackChain[originalPool]...), "")
	for _, fallbackPool := range fallbacks {
		slog.Warn("llm_router: target pool exhausted, attempting cross-pool fallback",
			"original_pool", originalPool, "fallback_pool", fallbackPool)
		// 克隆请求并切换目标 Pool，不修改其他任何推理参数
		degradedReq := *req
		degradedReq.ModelPool = fallbackPool

		ir.registry.mu.RLock()
		entry := ir.findBestProviderLockedMultiSkip(&degradedReq, nil)
		ir.registry.mu.RUnlock()
		if entry == nil {
			slog.Warn("llm_router: no available provider in fallback pool, trying next",
				"fallback_pool", fallbackPool)
			continue
		}
		start := time.Now()
		resp, err := entry.provider.Infer(ctx, msgs, opts...)
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
		if err == nil && resp != nil {
			ir.recordFailoverMetrics(ctx, entry, resp, start)
			// 标记发生了 Pool 降级，让上层感知（如 SessionOrchestrator 发送系统通知）
			resp.DegradedFromPool = originalPool
			slog.Info("llm_router: cross-pool degraded inference succeeded",
				"original_pool", originalPool, "actual_pool", fallbackPool)
			return resp, nil
		}
		slog.Warn("llm_router: fallback pool also failed, trying next",
			"fallback_pool", fallbackPool, "err", err)
	}
	// 所有 Pool（含降级）均耗尽
	return nil, apperr.Wrap(apperr.CodeResourceExhausted,
		"inference_router: all providers exhausted including fallback pools for "+originalPool,
		protocol.ErrAllProvidersFailed)
}

func (ir *InferenceRouter) findBestProviderLockedMultiSkip(req *types.InferRequest, skipped map[string]struct{}) *providerEntry {
	bestScore := -1.0
	var chosen *providerEntry
	for name, e := range ir.registry.entries {
		if _, skip := skipped[name]; skip || !e.cb.Allow() {
			continue
		}
		if req != nil {
			caps := e.provider.Capabilities()
			if (req.HasImageParts() && !caps.SupportsVision) || (req.HasVideoParts() && !caps.SupportsVideo) {
				continue
			}
			// ModelPool 非空时严格按 role 过滤：只考虑 role 与 ModelPool 完全匹配的 Provider。
			// "general" 不会自动透传到其他 Pool 的搜索结果；跨 Pool 降级由 tryPoolFallback 显式处理（GD-13-005）。
			if req.ModelPool != "" && e.role != req.ModelPool {
				continue
			}
		}
		if s := e.healthScore(); s > bestScore {
			bestScore = s
			chosen = e
		}
	}
	return chosen
}

func (ir *InferenceRouter) recordFailoverMetrics(ctx context.Context, chosen *providerEntry, resp *types.ProviderResponse, start time.Time) {
	caps := chosen.provider.Capabilities()
	costUSD := float64(resp.Usage.InputTokens)*caps.CostPer1KInput/1000.0 +
		float64(resp.Usage.OutputTokens)*caps.CostPer1KOutput/1000.0 +
		float64(resp.Usage.CacheHitTokens)*caps.CostPer1KCacheHit/1000.0
	ms := float64(time.Since(start).Milliseconds())
	trace.RecordLLMCall(ctx,
		chosen.name, resp.Model, "failover", ms,
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheHitTokens,
		costUSD,
	)
	// 2026-07-08 移除 eventWriter 写事件分支，理由同 router.go Infer() 的对应
	// 注释：protocol.EventWriter 零实现、恒不可达，观测已由上面的 trace.RecordLLMCall
	// 覆盖。详见 local_playground/reports/phase4-hard-dep-and-deadcode-followup-20260708.md。
}

// ClearBytes API Key 使用后原地清零（防止 heap dump 泄漏敏感数据）。
func ClearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (ir *InferenceRouter) acquireLLMCapacity(ctx context.Context) error {
	if ir.governor == nil {
		return nil
	}
	admitted, _ := ir.governor.AdmitLLM(1)
	if admitted {
		return nil
	}
	err := ir.governor.WaitForLLMCapacity(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: timeout waiting for LLM capacity", err).WithRetryAfter(10)
	}
	admitted, _ = ir.governor.AdmitLLM(1)
	if !admitted {
		return apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: failed to acquire LLM capacity", nil).WithRetryAfter(10)
	}
	return nil
}
