package llmgen

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── 阶段03 R-06：StructuredGenerator 回归测试 ─────────────────────────────
//
// 全测试文件共用一个进程级 tracer/meter provider：metrics.InitMetrics 内部由
// sync.Once 保护，仅第一次调用生效，故在 TestMain 里统一装配一次，之后每个
// 用例通过 reader.Collect 读取累计值并用"调用前后差值"断言，而非绝对值
// （同进程内多个用例共享同一累计计数器）。

var (
	testSpanExporter *tracetest.InMemoryExporter
	testMetricReader *sdkmetric.ManualReader
)

func TestMain(m *testing.M) {
	testSpanExporter = tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(testSpanExporter))
	otel.SetTracerProvider(tp)

	testMetricReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader))
	_ = metrics.InitMetrics(mp.Meter("llmgen_test"))

	m.Run()
}

// fastBackoff 避免测试真实等待 DefaultBackoff() 的秒级退避。
func fastBackoff() llmparent.BackoffConfig {
	return llmparent.BackoffConfig{Base: time.Millisecond, Max: 5 * time.Millisecond, JitterRatio: 0.1}
}

type testPayload struct {
	A int `json:"a"`
}

func validateTestPayload(dst *testPayload) ValidateFunc {
	return func(raw string) error {
		var p testPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "invalid json", err)
		}
		if p.A == 0 {
			return apperr.New(apperr.CodeInvalidInput, "missing field a")
		}
		*dst = p
		return nil
	}
}

// TestStructuredGenerator_RetriesThenSucceeds 验证首次返回损坏的 JSON、第二次
// 返回合法 JSON 时，最终成功且恰好调用 2 次。
func TestStructuredGenerator_RetriesThenSucceeds(t *testing.T) {
	g := NewStructuredGenerator("test_retry_success")
	g.backoff = fastBackoff()

	callCount := 0
	responses := []string{
		"```json\n{\"a\": 1", // 损坏：markdown 包裹但截断，非法 JSON
		`{"a": 1}`,           // 合法
	}
	gen := func(_ context.Context, _, _ string) (string, error) {
		defer func() { callCount++ }()
		return responses[callCount], nil
	}

	var got testPayload
	err := g.Generate(context.Background(), "sys", "intent", gen, validateTestPayload(&got))
	if err != nil {
		t.Fatalf("期望成功，实际报错: %v", err)
	}
	if callCount != 2 {
		t.Errorf("期望恰好调用 2 次，实际 %d 次", callCount)
	}
	if got.A != 1 {
		t.Errorf("期望解析出 A=1，实际 %+v", got)
	}
}

// TestStructuredGenerator_ExhaustsRetries_ExactlyThreeCalls 验证 LLM 恒返回垃圾
// 时，恰好调用 3 次（1 次初始 + maxStructuredRetries=2 次重试）后返回 error，
// 而非无限重试。
func TestStructuredGenerator_ExhaustsRetries_ExactlyThreeCalls(t *testing.T) {
	g := NewStructuredGenerator("test_exhaust")
	g.backoff = fastBackoff()

	callCount := 0
	gen := func(_ context.Context, _, _ string) (string, error) {
		callCount++
		return "not json at all", nil
	}
	var got testPayload
	err := g.Generate(context.Background(), "sys", "intent", gen, validateTestPayload(&got))
	if err == nil {
		t.Fatal("期望返回 error，实际 nil")
	}
	if !apperr.IsCode(err, apperr.CodeInternal) {
		t.Errorf("期望 CodeInternal，实际: %v", err)
	}
	if callCount != maxStructuredRetries+1 {
		t.Errorf("期望恰好调用 %d 次，实际 %d 次", maxStructuredRetries+1, callCount)
	}
}

// TestStructuredGenerator_CircuitBreaker_OpensAfterConsecutiveFailures 验证连续
// breakerThreshold(5) 次调用全部重试耗尽后，熔断开启：下一次调用被立即拒绝，
// 不发生任何 LLM 调用。
func TestStructuredGenerator_CircuitBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	g := NewStructuredGenerator("test_breaker")
	g.backoff = fastBackoff()

	callCount := 0
	failingGen := func(_ context.Context, _, _ string) (string, error) {
		callCount++
		return "garbage", nil
	}
	var sink testPayload

	for i := 0; i < breakerThreshold; i++ {
		err := g.Generate(context.Background(), "sys", "intent", failingGen, validateTestPayload(&sink))
		if err == nil {
			t.Fatalf("第 %d 次调用期望失败（构造的 gen 恒返回垃圾），实际成功", i+1)
		}
	}
	if callCount != breakerThreshold*(maxStructuredRetries+1) {
		t.Fatalf("熔断开启前期望累计调用 %d 次，实际 %d 次", breakerThreshold*(maxStructuredRetries+1), callCount)
	}

	// 第 6 轮：熔断应已开启，Generate 应立即拒绝，不再调用 gen。
	beforeCall := callCount
	err := g.Generate(context.Background(), "sys", "intent", failingGen, validateTestPayload(&sink))
	if err == nil {
		t.Fatal("熔断开启后期望立即拒绝，实际成功")
	}
	if !apperr.IsCode(err, apperr.CodeResourceExhausted) {
		t.Errorf("熔断拒绝期望 CodeResourceExhausted，实际: %v", err)
	}
	if callCount != beforeCall {
		t.Errorf("熔断开启后不应再调用 gen：调用前 %d 次，调用后 %d 次", beforeCall, callCount)
	}
}

// TestStructuredGenerator_RecordsSpanAndMetrics 验证一次成功调用会产生
// "extension.generate_<kind>" span，且 polaris.extension.llm_calls_total
// counter 增加（用 sdktrace 内存 exporter + sdkmetric ManualReader 断言，
// 均通过 TestMain 统一装配的 provider）。
func TestStructuredGenerator_RecordsSpanAndMetrics(t *testing.T) {
	const kind = "test_observability"
	g := NewStructuredGenerator(kind)
	g.backoff = fastBackoff()

	before := collectExtensionLLMCallsTotal(t, kind)
	testSpanExporter.Reset()

	gen := func(_ context.Context, _, _ string) (string, error) { return `{"a":1}`, nil }
	var got testPayload
	if err := g.Generate(context.Background(), "sys", "intent", gen, validateTestPayload(&got)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := testSpanExporter.GetSpans()
	found := false
	for _, s := range spans {
		if s.Name == "extension.generate_"+kind {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("期望找到名为 extension.generate_%s 的 span，实际 spans: %+v", kind, spanNames(spans))
	}

	after := collectExtensionLLMCallsTotal(t, kind)
	if after != before+1 {
		t.Errorf("期望 polaris.extension.llm_calls_total{kind=%s} 增加 1（%d → %d)", kind, before, after)
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

// collectExtensionLLMCallsTotal 读取当前 polaris.extension.llm_calls_total
// 在给定 kind label 下的累计值（跨全部 result label 求和）。
func collectExtensionLLMCallsTotal(t *testing.T, kind string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("metric collect failed: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "polaris.extension.llm_calls_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("kind")); ok && v.AsString() == kind {
					total += dp.Value
				}
			}
		}
	}
	return total
}
