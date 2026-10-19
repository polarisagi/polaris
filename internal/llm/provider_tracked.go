package llm

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type trackedProvider struct {
	protocol.Provider
	entry    *providerEntry
	registry *ProviderRegistry
}

func (tp *trackedProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (resp *types.ProviderResponse, err error) {
	defer func() {
		// 请求侧故障不说明 Provider 不健康，与路由的 recordAttempt 同一口径。
		if isRequestFault(err, tp.entry.name) {
			return
		}
		tp.entry.recordOutcome(err == nil, func() {
			tp.registry.mu.RLock()
			fn := tp.registry.onRecovery
			name := tp.entry.name
			tp.registry.mu.RUnlock()
			if fn != nil {
				fn(name)
			}
		})
	}()
	return tp.Provider.Infer(ctx, msgs, opts...) //nolint:wrapcheck
}

func (tp *trackedProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (ch <-chan types.StreamEvent, err error) {
	defer func() {
		// 请求侧故障不说明 Provider 不健康，与路由的 recordAttempt 同一口径。
		if isRequestFault(err, tp.entry.name) {
			return
		}
		tp.entry.recordOutcome(err == nil, func() {
			tp.registry.mu.RLock()
			fn := tp.registry.onRecovery
			name := tp.entry.name
			tp.registry.mu.RUnlock()
			if fn != nil {
				fn(name)
			}
		})
	}()
	return tp.Provider.StreamInfer(ctx, msgs, opts...) //nolint:wrapcheck
}
