package safecall

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Infer 是全仓唯一允许直接调用 provider.Infer 的入口。
// 强制超时兜底、resp/err 双重判空、Metrics/OTel 埋点；schema 校验通过 opts 可选开启。
func Infer(ctx context.Context, provider protocol.Provider, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	timeoutSec := 15
	cfg := config.Get()
	if cfg != nil && cfg.Thresholds.M1Router.SafecallInferTimeoutSeconds > 0 {
		timeoutSec = cfg.Thresholds.M1Router.SafecallInferTimeoutSeconds
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	ctx, span := otel.Tracer("llm").Start(ctx, "safecall.Infer")
	defer span.End()

	resp, err := provider.Infer(ctx, msgs, opts...)

	if metrics.InstrLLMCallsTotal != nil {
		metrics.InstrLLMCallsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("model", provider.ModelID())))
	}
	if metrics.InstrLLMLatencyMs != nil {
		metrics.InstrLLMLatencyMs.Record(ctx, float64(time.Since(start).Milliseconds()), metric.WithAttributes(attribute.String("model", provider.ModelID())))
	}

	if err != nil {
		return nil, apperr.Wrap(apperr.CodeProviderExhausted, "llm infer failed", err)
	}
	if resp == nil {
		return nil, apperr.New(apperr.CodeInternal, "llm infer returned nil response with nil error")
	}
	return resp, nil
}

// StreamInfer 也是全仓唯一允许直接调用 provider.StreamInfer 的入口。
func StreamInfer(ctx context.Context, provider protocol.Provider, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	timeoutSec := 60
	cfg := config.Get()
	if cfg != nil && cfg.Thresholds.M1Router.SafecallStreamIdleTimeoutSec > 0 {
		timeoutSec = cfg.Thresholds.M1Router.SafecallStreamIdleTimeoutSec
	}

	ctx, span := otel.Tracer("llm").Start(ctx, "safecall.StreamInfer")

	start := time.Now()
	evChan, err := provider.StreamInfer(ctx, msgs, opts...)

	if metrics.InstrLLMCallsTotal != nil {
		metrics.InstrLLMCallsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("model", provider.ModelID())))
	}
	if err != nil {
		span.End()
		return nil, apperr.Wrap(apperr.CodeProviderExhausted, "llm streaminfer failed", err)
	}
	if evChan == nil {
		span.End()
		return nil, apperr.New(apperr.CodeInternal, "llm streaminfer returned nil channel with nil error")
	}

	outChan := make(chan types.StreamEvent)
	concurrent.SafeGo(ctx, "safecall.StreamInfer", func(ctx context.Context) {
		defer span.End()
		defer close(outChan)
		if metrics.InstrLLMLatencyMs != nil {
			defer func() {
				metrics.InstrLLMLatencyMs.Record(ctx, float64(time.Since(start).Milliseconds()), metric.WithAttributes(attribute.String("model", provider.ModelID())))
			}()
		}

		idleTimeout := time.Duration(timeoutSec) * time.Second
		timer := time.NewTimer(idleTimeout)
		defer timer.Stop()

		for {
			timer.Reset(idleTimeout)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				outChan <- types.StreamEvent{
					Type:    types.StreamError,
					Content: "stream idle timeout",
				}
				return
			case ev, ok := <-evChan:
				if !ok {
					return
				}
				// We don't block forever writing to outChan in case the receiver goes away
				select {
				case <-ctx.Done():
					return
				case outChan <- ev:
				}
			}
		}
	})

	return outChan, nil
}
