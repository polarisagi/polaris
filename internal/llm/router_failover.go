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
			return nil, apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: all providers exhausted", protocol.ErrAllProvidersFailed)
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
		if err == nil && resp != nil {
			ir.recordFailoverMetrics(ctx, chosen, resp, start)
			return resp, nil
		}
		if ce := ClassifyWithProvider(err, chosen.name); !ce.Retryable && !ce.ShouldFallback {
			slog.Warn("inference_router: non-retryable error during failover, aborting remaining attempts",
				"provider", chosen.name, "reason", ce.Reason, "err", err, "tried", len(skipped)+1)
			return nil, apperr.Wrap(apperr.CodeInternal, "InferenceRouter.failover: non-retryable ("+string(ce.Reason)+")", err)
		}
		skipped[chosen.name] = struct{}{}
		slog.Warn("inference_router: failover attempt failed, trying next",
			"provider", chosen.name, "err", err, "tried", len(skipped))
	}
}

//nolint:unused // 保留字段：迁移前即无调用方（原文件 new-from-rev 豁免掩盖），非本次拆分引入，不在本次改动范围内删除
func (ir *InferenceRouter) findBestProviderLocked(req *types.InferRequest, skip string) *providerEntry {
	skipped := map[string]struct{}{skip: {}}
	return ir.findBestProviderLockedMultiSkip(req, skipped)
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
		return apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: timeout waiting for LLM capacity", err)
	}
	admitted, _ = ir.governor.AdmitLLM(1)
	if !admitted {
		return apperr.Wrap(apperr.CodeResourceExhausted, "inference_router: failed to acquire LLM capacity", nil)
	}
	return nil
}
