package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/observability/metrics"
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
	// Prometheus 在这里打点而非 fillUsage：流式调用每个带用量的事件都会调 fillUsage（取最后一个非零值），
	// 只有 push 时的行才是终态，才不会重复累计；先于入队，队列满丢行也不丢指标（记账可丢，指标不该少算）。
	metrics.RecordLLMPromptTokens(context.Background(), row.Purpose, row.Provider, row.InputTokens, row.CacheHitTokens)
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
	// cache 为 nil（测试直接构造）或其指向 nil 时不缓存。
	cache *atomic.Pointer[responseCache]
	// inputExcludesCache：该 Provider 的 InputTokens 不含缓存命中部分（Anthropic 语义：input_tokens 只计
	// 未命中缓存的输入，cache_read/cache_creation 另计）。OpenAI/DeepSeek/Google 的 prompt_tokens 已含命中。
	inputExcludesCache bool
}

// usageExcludesCache 判定 Provider 的 Usage.InputTokens 口径。适配器（internal/llm/adapter）不可依赖，
// 也没有口径声明字段，这里按具体类型名识别 Anthropic 适配器；运行期另有 CacheCreationTokens>0 的
// 口径信号兜底（只有 Anthropic 会填该字段）。
func usageExcludesCache(p protocol.Provider) bool {
	return strings.Contains(fmt.Sprintf("%T", p), "AnthropicAdapter")
}

func (p *usageRecordingProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	start := time.Now()
	// 精确响应缓存（ADR-0105 决策六）：命中时不调用 Provider，只记一行 status=cache_hit、token 为 0 的 llm_calls。
	rc, cc, hit := p.probeResponseCache(ctx, msgs, opts)
	if hit != nil {
		if sink := p.sink.Load(); sink != nil {
			row := p.baseRecord(ctx, start, opts, false)
			row.Status = protocol.LLMUsageStatusCacheHit
			sink.push(row)
		}
		return hit, nil
	}
	resp, err := p.Provider.Infer(ctx, msgs, opts...)
	if err == nil && cc != nil {
		rc.put(ctx, cc, resp)
	}
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
//
// 口径归一（llm_calls.input_tokens 一律 = 全部输入，含缓存命中，见 039_llm_calls.sql）：
//   - OpenAI/DeepSeek/Google 的 toUsage 直接给出含命中的 prompt_tokens，原样写入；
//   - Anthropic 的 input_tokens 只计未命中缓存的部分，cache_read/cache_creation 另计，
//     必须加回才与其余 Provider 同口径——否则 cache_hit/input 会 >1，且下面的 miss 被夹成 0、
//     按输入计费的部分整体漏算。cache_creation 按输入费率计（Anthropic 实际为 1.25x/2x，估算取下界）。
func (p *usageRecordingProvider) fillUsage(row *protocol.LLMUsageRecord, u types.Usage) {
	input, miss := u.InputTokens, max(u.InputTokens-u.CacheHitTokens, 0)
	if p.inputExcludesCache || u.CacheCreationTokens > 0 {
		input = u.InputTokens + u.CacheHitTokens + u.CacheCreationTokens
		miss = u.InputTokens + u.CacheCreationTokens
	}
	row.InputTokens, row.CacheHitTokens = input, u.CacheHitTokens
	row.OutputTokens, row.ReasoningTokens = u.OutputTokens, u.ReasoningTokens
	caps := p.Capabilities()
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
