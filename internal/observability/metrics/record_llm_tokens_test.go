package metrics

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// sumByPurposeProvider 读取某 Int64 Sum 指标按 purpose/provider 拆分的累计值。
func sumByPurposeProvider(t *testing.T, r *sdkmetric.ManualReader, name string) map[[2]string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[[2]string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s 不是 Int64 Sum", name)
			}
			for _, dp := range sum.DataPoints {
				p, _ := dp.Attributes.Value("purpose")
				pr, _ := dp.Attributes.Value("provider")
				out[[2]string{p.AsString(), pr.AsString()}] += dp.Value
			}
		}
	}
	return out
}

// ADR-0105 决策八：input / cache_hit 两个 counter 按 purpose+provider 切分，命中率由查询侧相除。
func TestRecordLLMPromptTokens_SplitsByPurposeAndProvider(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")
	prevIn, prevHit := InstrLLMInputTokens, InstrLLMCacheHitTokens
	t.Cleanup(func() { InstrLLMInputTokens, InstrLLMCacheHitTokens = prevIn, prevHit })
	var err error
	if InstrLLMInputTokens, err = meter.Int64Counter("polaris.llm.input_tokens_total"); err != nil {
		t.Fatal(err)
	}
	if InstrLLMCacheHitTokens, err = meter.Int64Counter("polaris.llm.cache_hit_tokens_total"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	RecordLLMPromptTokens(ctx, "plan", "deepseek", 1000, 900)
	RecordLLMPromptTokens(ctx, "plan", "deepseek", 500, 0) // 未命中：input 计入，cache_hit 不出现
	RecordLLMPromptTokens(ctx, "respond", "anthropic", 200, 50)
	RecordLLMPromptTokens(ctx, "plan", "deepseek", 0, 0) // 无输入（错误行/缓存命中行）不打点

	in := sumByPurposeProvider(t, reader, "polaris.llm.input_tokens_total")
	hit := sumByPurposeProvider(t, reader, "polaris.llm.cache_hit_tokens_total")
	if in[[2]string{"plan", "deepseek"}] != 1500 || in[[2]string{"respond", "anthropic"}] != 200 {
		t.Fatalf("input 累计错误: %v", in)
	}
	if hit[[2]string{"plan", "deepseek"}] != 900 || hit[[2]string{"respond", "anthropic"}] != 50 {
		t.Fatalf("cache_hit 累计错误: %v", hit)
	}
}

// instrument 为 nil（Tier-0 无 OTel）时静默跳过，不得 panic。
func TestRecordLLMPromptTokens_NilInstrumentsSafe(t *testing.T) {
	prevIn, prevHit := InstrLLMInputTokens, InstrLLMCacheHitTokens
	t.Cleanup(func() { InstrLLMInputTokens, InstrLLMCacheHitTokens = prevIn, prevHit })
	InstrLLMInputTokens, InstrLLMCacheHitTokens = nil, nil
	RecordLLMPromptTokens(context.Background(), "plan", "x", 10, 5)
}
