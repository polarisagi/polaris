package adapter

import (
	"context"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// relayStreamEvents 把解码协程产出的 rawCh 转发到对外的 outCh，并统计 token 燃烧速率与缓存命中。
//
// cancel（可为 nil）必须在 rawCh 排空之后才触发：此前由解码协程在写完最后一帧后立刻 cancel，
// 转发处 `select { outCh<-ev; <-ctx.Done() }` 两路同时就绪时随机选择，约一半概率丢弃 rawCh 里尚未
// 转发的 tool_calls 与末尾 usage（空输出根因，见 M01 §5.3-bis）。
// addTokens 为 nil 表示不统计燃烧速率。
func relayStreamEvents(
	ctx context.Context, rawCh <-chan types.StreamEvent, outCh chan<- types.StreamEvent,
	provider, model string, addTokens func(int64), cancel context.CancelFunc,
) {
	if cancel != nil {
		defer cancel()
	}
	for ev := range rawCh {
		// 仅末事件（收到完整 usage 后）才填充 InputTokens/OutputTokens，中间 delta 为 0，不会重复计数。
		if addTokens != nil && (ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0) {
			addTokens(burnTokens(ev.Usage))
		}
		if ev.Usage.CacheHitTokens > 0 || ev.Usage.InputTokens > 0 {
			metrics.RecordLLMCacheHit(provider, model, ev.Usage.CacheHitTokens > 0)
		}
		select {
		case outCh <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// burnTokens 返回计入 TokenBurnRate 的 token 数：非缓存输入 + 输出。
// 燃烧率用于成本熔断，缓存命中价约为未命中的 1/50（ADR-0120 决策四），按全额计会把带缓存的正常长对话判为失控。
// 仅适用于 InputTokens 已含缓存命中的 provider（OpenAI/DeepSeek/Google）；Anthropic 的 input_tokens 本就不含 cache_read，
// 无缓存字段的 provider（local/ollama）CacheHitTokens 恒为 0，同样适用。
func burnTokens(u types.Usage) int64 {
	return int64(max(u.InputTokens-u.CacheHitTokens, 0) + u.OutputTokens)
}
