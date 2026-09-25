package llm

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// UsageRecorder 把一次 LLM 调用的记账行落库（llm_calls 表）。接口定义在消费方（HE-3），
// 生产实现为 internal/store/repo.SQLiteLLMCallRepository。
type UsageRecorder interface {
	RecordLLMUsage(ctx context.Context, rec protocol.LLMUsageRecord) error
}

// usageQueueSize 记账队列容量。满时丢弃并计数：记账不得阻塞推理，也不得让慢存储
// 拖住 Provider 调用（SQLite 单写者）。
const usageQueueSize = 512

type usageSink struct {
	ch      chan protocol.LLMUsageRecord
	dropped atomic.Int64
}

// InjectUsageRecorder 启用 llm_calls 记账：此后经本注册表任一 Provider 发起的调用
// （路由主路径、failover、降级池、溢出升级、PickProvider 直取）都会写一行。
// 单个写协程随 ctx 结束。未注入时调用照常进行，只是不记账。
func (r *ProviderRegistry) InjectUsageRecorder(ctx context.Context, rec UsageRecorder) {
	if rec == nil {
		return
	}
	sink := &usageSink{ch: make(chan protocol.LLMUsageRecord, usageQueueSize)}
	r.usage.Store(sink)
	concurrent.SafeGo(ctx, "llm.usage_recorder", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case row := <-sink.ch:
				if err := rec.RecordLLMUsage(context.WithoutCancel(ctx), row); err != nil {
					slog.Warn("llm usage: record failed", "provider", row.Provider, "model", row.ModelID, "err", err)
				}
			}
		}
	})
}

func (s *usageSink) push(row protocol.LLMUsageRecord) {
	select {
	case s.ch <- row:
	default:
		if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("llm usage: queue full, record dropped", "dropped_total", n)
		}
	}
}

// usageRecordingProvider 为注册表中的每个 Provider 记账。只拦截 Infer/StreamInfer，其余方法
// 经嵌入透传；需要具体类型断言的调用方经 ProviderRegistry.Get 取原始实例。
type usageRecordingProvider struct {
	protocol.Provider
	name string
	sink *atomic.Pointer[usageSink]
}

func (p *usageRecordingProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	start := time.Now()
	resp, err := p.Provider.Infer(ctx, msgs, opts...)
	if sink := p.sink.Load(); sink != nil {
		row := p.baseRecord(ctx, start, opts, false)
		if resp != nil {
			p.fillUsage(&row, resp.Usage)
		}
		setStatus(ctx, &row, err)
		sink.push(row)
	}
	return resp, err //nolint:wrapcheck // 透明包装：错误链须原样交给路由分类（Classify / errors.Is）
}

func (p *usageRecordingProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	start := time.Now()
	ch, err := p.Provider.StreamInfer(ctx, msgs, opts...)
	sink := p.sink.Load()
	if sink == nil {
		return ch, err //nolint:wrapcheck // 同 Infer
	}
	if err != nil {
		row := p.baseRecord(ctx, start, opts, true)
		setStatus(ctx, &row, err)
		sink.push(row)
		return nil, err //nolint:wrapcheck // 同 Infer
	}
	out := make(chan types.StreamEvent)
	concurrent.SafeGo(ctx, "llm.usage_recorder.stream", func(ctx context.Context) {
		defer close(out)
		row := p.baseRecord(ctx, start, opts, true)
		row.Status = protocol.LLMUsageStatusOK
		defer func() {
			row.LatencyMs = time.Since(start).Milliseconds()
			sink.push(row)
		}()
		for ev := range ch {
			observeStreamEvent(&row, ev, p)
			select {
			case out <- ev:
			case <-ctx.Done():
				row.Status = protocol.LLMUsageStatusCancelled
				return
			}
		}
	})
	return out, nil
}

// observeStreamEvent 从流事件提取终态与用量。用量取最后一个非零 Usage：OpenAI 兼容流在
// 末尾块报告全量 usage，取消时适配器补发估算值。
func observeStreamEvent(row *protocol.LLMUsageRecord, ev types.StreamEvent, p *usageRecordingProvider) {
	if u := ev.Usage; u.InputTokens > 0 || u.OutputTokens > 0 {
		p.fillUsage(row, u)
	}
	switch ev.Type {
	case types.StreamError:
		row.Status, row.Error = protocol.LLMUsageStatusError, truncateErr(ev.Content)
	case types.StreamCancelled:
		row.Status = protocol.LLMUsageStatusCancelled
	}
}

func (p *usageRecordingProvider) baseRecord(ctx context.Context, start time.Time, opts []types.InferOption, streaming bool) protocol.LLMUsageRecord {
	var o types.InferOptions
	for _, fn := range opts {
		fn(&o)
	}
	model := o.Model
	if model == "" {
		model = p.ModelID()
	}
	purpose := o.Purpose
	if purpose == "" {
		purpose = "unspecified"
	}
	sessionID, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	return protocol.LLMUsageRecord{
		ID:           uuid.NewString(),
		CreatedAtMs:  start.UnixMilli(),
		SessionID:    sessionID,
		Purpose:      purpose,
		Provider:     p.name,
		ModelID:      model,
		ModelPool:    o.ModelPool,
		ThinkingMode: string(o.ThinkingMode),
		Streaming:    streaming,
		LatencyMs:    time.Since(start).Milliseconds(),
	}
}

// fillUsage 写入用量与按 ProviderCapabilities 费率（每 1K token）估算的费用。
func (p *usageRecordingProvider) fillUsage(row *protocol.LLMUsageRecord, u types.Usage) {
	row.InputTokens, row.CacheHitTokens = u.InputTokens, u.CacheHitTokens
	row.OutputTokens, row.ReasoningTokens = u.OutputTokens, u.ReasoningTokens
	caps := p.Capabilities()
	miss := max(u.InputTokens-u.CacheHitTokens, 0)
	row.CostUSD = (float64(miss)*caps.CostPer1KInput +
		float64(u.CacheHitTokens)*caps.CostPer1KCacheHit +
		float64(u.OutputTokens)*caps.CostPer1KOutput) / 1000
}

func setStatus(ctx context.Context, row *protocol.LLMUsageRecord, err error) {
	switch {
	case err == nil:
		row.Status = protocol.LLMUsageStatusOK
	case ctx.Err() != nil:
		row.Status, row.Error = protocol.LLMUsageStatusCancelled, truncateErr(err.Error())
	default:
		row.Status, row.Error = protocol.LLMUsageStatusError, truncateErr(err.Error())
	}
}

func truncateErr(s string) string {
	const maxErrBytes = 512
	return util.ElideMiddle(s, maxErrBytes)
}
