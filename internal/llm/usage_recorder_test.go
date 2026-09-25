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
