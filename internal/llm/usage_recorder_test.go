package llm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type collectingRecorder struct {
	mu   sync.Mutex
	rows []protocol.LLMUsageRecord
}

func (c *collectingRecorder) RecordLLMUsage(_ context.Context, rec protocol.LLMUsageRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, rec)
	return nil
}

func (c *collectingRecorder) waitRows(t *testing.T, n int) []protocol.LLMUsageRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.rows) >= n {
			rows := append([]protocol.LLMUsageRecord(nil), c.rows...)
			c.mu.Unlock()
			return rows
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d usage rows", n)
	return nil
}

// usageProvider 非流式返回固定用量；流式在末尾块报告全量 usage（OpenAI 兼容流的形态）。
type usageProvider struct{ mockProvider }

func (p *usageProvider) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "ok", Usage: types.Usage{InputTokens: 1000, CacheHitTokens: 900, OutputTokens: 200, ReasoningTokens: 150}}, nil
}

func (p *usageProvider) StreamInfer(context.Context, []types.Message, ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "ok"}
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Usage: types.Usage{InputTokens: 500, CacheHitTokens: 400, OutputTokens: 50}}
	close(ch)
	return ch, nil
}

// 每次经注册表发起的调用都须写一行：用途、会话、模型、token 明细与费用（ADR-0101 决策六）。
func TestUsageRecorder_RecordsInferAndStream(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	p := &usageProvider{mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0.3, CostPer1KOutput: 1.2, CostPer1KCacheHit: 0.006}}}
	reg.RegisterWithRole("flash", "Flash", "default", p)
	rec := &collectingRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.InjectUsageRecorder(ctx, rec)
	router := NewInferenceRouter(reg, nil)

	callCtx := context.WithValue(ctx, protocol.CtxTaskIDKey{}, "sess-1")
	msgs := []types.Message{{Role: "user", Content: "hi"}}
	if _, err := router.Infer(callCtx, msgs, types.WithPurpose("plan"), types.WithModelPool("default"), types.WithThinkingMode(types.ThinkingHigh)); err != nil {
		t.Fatal(err)
	}
	ch, err := router.StreamInfer(callCtx, msgs, types.WithPurpose("respond"), types.WithModelPool("default"))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	rows := rec.waitRows(t, 2)
	byPurpose := map[string]protocol.LLMUsageRecord{}
	for _, r := range rows {
		byPurpose[r.Purpose] = r
	}
	plan, respond := byPurpose["plan"], byPurpose["respond"]
	if plan.SessionID != "sess-1" || plan.Provider != "flash" || plan.ModelID != "mock" || plan.ModelPool != "default" ||
		plan.ThinkingMode != "high" || plan.Streaming || plan.Status != protocol.LLMUsageStatusOK {
		t.Fatalf("plan row metadata: %+v", plan)
	}
	if plan.InputTokens != 1000 || plan.CacheHitTokens != 900 || plan.OutputTokens != 200 || plan.ReasoningTokens != 150 {
		t.Fatalf("plan row tokens: %+v", plan)
	}
	// 100 未命中×0.3 + 900 命中×0.006 + 200 输出×1.2，按每 1K 计价。
	if want := (100*0.3 + 900*0.006 + 200*1.2) / 1000; plan.CostUSD < want-1e-9 || plan.CostUSD > want+1e-9 {
		t.Fatalf("plan cost %v want %v", plan.CostUSD, want)
	}
	if !respond.Streaming || respond.InputTokens != 500 || respond.CacheHitTokens != 400 || respond.OutputTokens != 50 {
		t.Fatalf("stream row: %+v", respond)
	}
}

func TestUsageRecorder_RecordsFailures(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	reg.Register("over", "Over", &overflowProvider{})
	rec := &collectingRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.InjectUsageRecorder(ctx, rec)
	_, _ = NewInferenceRouter(reg, nil).Infer(ctx, []types.Message{{Role: "user", Content: "hi"}})
	if row := rec.waitRows(t, 1)[0]; row.Status != protocol.LLMUsageStatusError || row.Error == "" || row.Purpose != "unspecified" {
		t.Fatalf("failure row: %+v", row)
	}
}

// 未指定池的请求须先选便宜档：即使贵档 Provider 的健康分更高。
func TestBestWith_PrefersCheapTier(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	reg.RegisterWithRole("pro", "Pro", "reasoning", &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 0}})
	reg.RegisterWithRole("flash", "Flash", "default", &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 9}})
	if e := reg.best(&types.InferRequest{}); e == nil || e.name != "flash" {
		t.Fatalf("unpooled request must pick cheap tier, got %v", e)
	}
	reg.Unregister("flash")
	if e := reg.best(&types.InferRequest{}); e == nil || e.name != "pro" {
		t.Fatal("expensive tier remains the fallback when no cheaper provider exists")
	}
}

// fakeAnthropicAdapter 的类型名含 "AnthropicAdapter"，走 usageExcludesCache 的类型识别；
// 用量按 Anthropic 语义：input_tokens 只计未命中缓存的部分。
type fakeAnthropicAdapter struct{ mockProvider }

func (p *fakeAnthropicAdapter) Infer(context.Context, []types.Message, ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "ok", Usage: types.Usage{InputTokens: 100, CacheHitTokens: 900, CacheCreationTokens: 0, OutputTokens: 200}}, nil
}

// llm_calls.input_tokens 必须统一为"含缓存命中"的口径：Anthropic 的 input 需加回 cache_read/cache_creation，
// 否则 cache_hit/input 会 >1，且输入费用被夹成 0 漏算。
func TestUsageRecorder_NormalizesAnthropicInputToIncludeCache(t *testing.T) {
	reg := NewProviderRegistry(config.M1RouterThresholds{})
	reg.Register("claude", "Claude", &fakeAnthropicAdapter{mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 3, CostPer1KOutput: 15, CostPer1KCacheHit: 0.3}}})
	rec := &collectingRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg.InjectUsageRecorder(ctx, rec)
	if _, err := NewInferenceRouter(reg, nil).Infer(ctx, []types.Message{{Role: "user", Content: "hi"}}, types.WithPurpose("plan")); err != nil {
		t.Fatal(err)
	}
	row := rec.waitRows(t, 1)[0]
	if row.InputTokens != 1000 || row.CacheHitTokens != 900 {
		t.Fatalf("Anthropic input 应归一为 100+900=1000：%+v", row)
	}
	// 100 未命中×3 + 900 命中×0.3 + 200 输出×15，每 1K。
	if want := (100*3 + 900*0.3 + 200*15) / 1000.0; row.CostUSD < want-1e-9 || row.CostUSD > want+1e-9 {
		t.Fatalf("cost %v want %v", row.CostUSD, want)
	}
}

// 口径信号兜底：任何 Provider 只要报告 CacheCreationTokens>0（只有 Anthropic 语义会填）也按"不含命中"处理，
// creation 计入输入并按输入费率计价。
func TestUsageRecorder_CacheCreationSignalsExcludedInput(t *testing.T) {
	p := &usageRecordingProvider{Provider: &mockProvider{caps: types.ProviderCapabilities{CostPer1KInput: 2}}}
	var row protocol.LLMUsageRecord
	p.fillUsage(&row, types.Usage{InputTokens: 10, CacheHitTokens: 20, CacheCreationTokens: 70})
	if row.InputTokens != 100 || row.CacheHitTokens != 20 {
		t.Fatalf("got %+v", row)
	}
	if want := 80 * 2 / 1000.0; row.CostUSD < want-1e-9 || row.CostUSD > want+1e-9 {
		t.Fatalf("cost %v want %v", row.CostUSD, want)
	}
	// OpenAI 语义（含命中）不受影响。
	var o protocol.LLMUsageRecord
	p.fillUsage(&o, types.Usage{InputTokens: 1000, CacheHitTokens: 900})
	if o.InputTokens != 1000 {
		t.Fatalf("含命中口径被误改: %+v", o)
	}
}
