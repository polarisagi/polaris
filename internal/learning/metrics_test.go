package learning

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

// ─── GD-13-005：Reflexion 信号量满丢弃计数可观测性回归测试 ──────────────────
//
// 全测试文件共用一个进程级 tracer/meter provider（同 llmgen 包既有模式，见
// internal/extension/llmgen/generate_structured_test.go 顶部注释）：
// metrics.InitMetrics 内部由 sync.Once 保护，仅第一次调用生效，故在 TestMain
// 里统一装配一次，之后每个用例通过 reader.Collect 读取累计值并用"调用前后
// 差值"断言，而非绝对值。

var testMetricReader *sdkmetric.ManualReader

func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	otel.SetTracerProvider(tp)

	testMetricReader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader))
	_ = metrics.InitMetrics(mp.Meter("learning_test"))

	m.Run()
}

// collectReflectionDroppedTotal 读取当前 polaris.learning.reflection_dropped_total 累计值。
func collectReflectionDroppedTotal(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("metric collect failed: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "polaris.learning.reflection_dropped_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// blockingReflector.Reflect 阻塞直到 release 被 close，用于在测试中稳定占满
// Engine.sem 的唯一槽位（MaxConcurrentReflections=1），驱动第二个失败事件
// 命中 handleTaskCompleteEvent 的 default 丢弃分支。
type blockingReflector struct {
	release chan struct{}
}

func (b *blockingReflector) Reflect(_ context.Context, _, _ string, _ *TaskResult, _ []Step, _ int) (*Reflection, error) {
	<-b.release
	return &Reflection{}, nil
}

// TestEngine_HandleTaskCompleteEvent_DropsWhenSemaphoreFull 复现 GD-13-005：
// 反思并发信号量满时，handleTaskCompleteEvent 此前静默丢弃失败任务事件，不计
// 入任何计数器。`e.sem <- struct{}{}` 在 select 分支中同步执行（先于 SafeGo
// 派生的 goroutine 启动），因此第一次调用返回时槽位已确定性地被占满，无需
// 额外同步即可保证第二次调用必然命中 default 分支。
func TestEngine_HandleTaskCompleteEvent_DropsWhenSemaphoreFull(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.MaxConcurrentReflections = 1

	release := make(chan struct{})
	defer close(release) // 测试结束后放行阻塞的 Reflect，避免 goroutine 泄漏

	e, _, _ := newTestEngine(cfg, &blockingReflector{release: release}, &mockCurriculum{}, &mockRollout{})

	before := collectReflectionDroppedTotal(t)

	ev1 := TaskCompleteEvent{TaskID: "t1", TaskType: "code_act", Success: false}
	ev2 := TaskCompleteEvent{TaskID: "t2", TaskType: "code_act", Success: false}

	e.handleTaskCompleteEvent(context.Background(), ev1) // 占满唯一信号量槽位
	e.handleTaskCompleteEvent(context.Background(), ev2) // 应命中 default 分支被丢弃

	after := collectReflectionDroppedTotal(t)
	if after != before+1 {
		t.Errorf("期望 polaris.learning.reflection_dropped_total 增加 1（%d → %d）", before, after)
	}
}

// TestEngine_HandleTaskCompleteEvent_NoDropUnderCapacity 回归验证：信号量未
// 满时不应计入丢弃计数（避免"逢失败必计数"的过度触发误报）。
func TestEngine_HandleTaskCompleteEvent_NoDropUnderCapacity(t *testing.T) {
	cfg := DefaultEngineConfig()
	cfg.MaxConcurrentReflections = 3

	release := make(chan struct{})
	defer close(release)

	e, _, _ := newTestEngine(cfg, &blockingReflector{release: release}, &mockCurriculum{}, &mockRollout{})

	before := collectReflectionDroppedTotal(t)

	e.handleTaskCompleteEvent(context.Background(), TaskCompleteEvent{TaskID: "t1", TaskType: "code_act", Success: false})

	after := collectReflectionDroppedTotal(t)
	if after != before {
		t.Errorf("信号量未满时不应触发丢弃计数（%d → %d）", before, after)
	}
}
