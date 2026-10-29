package llm

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// isRequestFault 报告 err 是否为请求侧故障（上下文超限 / payload 过大，Classify 以
// ShouldCompress 标记）。原样重发给同等容量的 Provider 必然再次失败且照样计费；
// 缩减请求只有持有消息语义的调用方能做。
func isRequestFault(err error, providerName string) bool {
	return err != nil && ClassifyWithProvider(err, providerName).ShouldCompress
}

// requestFaultError 把请求侧故障包装为 protocol.ErrContextOverflow，保留 Provider 原文供日志。
func requestFaultError(op, providerName string, err error) error {
	return apperr.Wrap(apperr.CodeInvalidInput,
		op+": request exceeds model limits at "+providerName+": "+err.Error(),
		protocol.ErrContextOverflow)
}

// recordAttempt 记录一次 Provider 调用对健康度（熔断器、成功率、模型注册表）的影响。
// 请求侧故障不说明 Provider 不健康，不计入：此前超长请求会逐个打开池内健康
// Provider 的熔断器，最终报"所有 Provider 耗尽"。
func (ir *InferenceRouter) recordAttempt(ctx context.Context, entry *providerEntry, err error) {
	if isRequestFault(err, entry.name) {
		return
	}
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
}

// largerWindowCandidate 选出上下文窗口严格大于 window 的最优 Provider：先在请求的 Pool
// 内选，没有再退到不限 role 的全局（与 tryPoolFallback 链尾同一兜底）。窗口未知（0）的
// Provider 不参选——无法判断它能否装下这次请求。窗口严格递增保证 failover 必然终止。
func (ir *InferenceRouter) largerWindowCandidate(req *types.InferRequest, window int) (*providerEntry, *types.InferRequest) {
	ir.registry.mu.RLock()
	defer ir.registry.mu.RUnlock()
	skip := make(map[string]struct{})
	for name, e := range ir.registry.entries {
		if e.provider.Capabilities().MaxContextTokens <= window {
			skip[name] = struct{}{}
		}
	}
	if c := ir.findBestProviderLockedMultiSkip(req, skip); c != nil {
		return c, req
	}
	if req.ModelPool == "" {
		return nil, nil
	}
	global := *req
	global.ModelPool = ""
	if c := ir.findBestProviderLockedMultiSkip(&global, skip); c != nil {
		return c, &global
	}
	return nil, nil
}

// overflowFailover 请求侧故障后的容量感知 failover：只转向窗口更大的 Provider
// （本地小窗口模型超限时自动升到云端大窗口模型）；没有更大的，或更大的也失败，
// 返回 ErrContextOverflow 交由调用方缩减请求。
func (ir *InferenceRouter) overflowFailover(ctx context.Context, msgs []types.Message, opts []types.InferOption,
	req *types.InferRequest, failed *providerEntry, cause error) (*types.ProviderResponse, error) {
	window := failed.provider.Capabilities().MaxContextTokens
	for ctx.Err() == nil {
		next, nreq := ir.largerWindowCandidate(req, window)
		if next == nil {
			break
		}
		start := time.Now()
		var resp *types.ProviderResponse
		var err error
		func() {
			defer func() { ir.recordAttempt(ctx, next, err) }()
			resp, err = next.provider.Infer(ctx, msgs, opts...)
		}()
		if err == nil && resp != nil {
			ir.recordFailoverMetrics(ctx, next, resp, start)
			if nreq.ModelPool != req.ModelPool && isCapabilityDowngrade(req.ModelPool) {
				resp.DegradedFromPool = req.ModelPool
			}
			slog.Info("inference_router: context overflow served by larger-window provider",
				"from", failed.name, "to", next.name, "window", next.provider.Capabilities().MaxContextTokens)
			return resp, nil
		}
		if !isRequestFault(err, next.name) {
			slog.Warn("inference_router: larger-window provider failed after context overflow",
				"provider", next.name, "err", err)
			break
		}
		failed, cause, window = next, err, next.provider.Capabilities().MaxContextTokens
	}
	return nil, requestFaultError("InferenceRouter.Infer", failed.name, cause)
}

// streamOverflowFailover overflowFailover 的流式对偶。
func (ir *InferenceRouter) streamOverflowFailover(ctx context.Context, msgs []types.Message, opts []types.InferOption,
	req *types.InferRequest, failed *providerEntry, cause error) (<-chan types.StreamEvent, error) {
	window := failed.provider.Capabilities().MaxContextTokens
	for ctx.Err() == nil {
		next, nreq := ir.largerWindowCandidate(req, window)
		if next == nil {
			break
		}
		var ch <-chan types.StreamEvent
		var err error
		func() {
			defer func() { ir.recordAttempt(ctx, next, err) }()
			ch, err = next.provider.StreamInfer(ctx, msgs, opts...)
		}()
		if err == nil {
			slog.Info("inference_router: stream context overflow served by larger-window provider",
				"from", failed.name, "to", next.name, "window", next.provider.Capabilities().MaxContextTokens)
			wrapped := ir.wrapStreamChannel(ctx, ch, nreq, next.name)
			if nreq.ModelPool != req.ModelPool && isCapabilityDowngrade(req.ModelPool) {
				return prependDegradeNotice(ctx, wrapped, req.ModelPool), nil
			}
			return wrapped, nil
		}
		if !isRequestFault(err, next.name) {
			slog.Warn("inference_router: larger-window provider failed after stream context overflow",
				"provider", next.name, "err", err)
			break
		}
		failed, cause, window = next, err, next.provider.Capabilities().MaxContextTokens
	}
	return nil, requestFaultError("InferenceRouter.StreamInfer", failed.name, cause)
}

// isCapabilityDowngrade 报告从 pool 回落是否意味着模型能力下降、值得告知用户。只有贵档
// reasoning 池回落才是；日常阶段请求的 default/general 池回落到其他档（例如用户只配了
// general 角色的模型）不是降级，提示"高阶推理模型不可用"属误报（ADR-0101 决策七）。
func isCapabilityDowngrade(pool string) bool {
	return types.ModelPool(pool) == types.ModelPoolReasoning
}
