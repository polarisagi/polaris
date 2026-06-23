package metrics

import (
	"context"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ── 同步 instruments（Counter / Histogram）─────────────────────────────────
// 全部包级 nil 变量；InitMetrics 赋值后方可使用。
// RecordXxx 函数在 nil 时静默返回（Tier-0 legacy 路径安全）。

var (
	// M1 LLM 调用
	InstrLLMCallsTotal                metric.Int64Counter
	InstrLLMLatencyMs                 metric.Float64Histogram
	InstrTokensTotal                  metric.Int64Counter
	InstrAPIcostUSD                   metric.Float64Counter
	InstrBurnStage3Total              metric.Int64Counter
	InstrLLMCacheHitRate              metric.Float64Histogram // (ISSUE-04)
	InstrEventBufferDrainTimeoutTotal metric.Int64Counter

	// M7 工具调用 & 沙箱
	InstrToolCallsTotal metric.Int64Counter
	InstrToolLatencyMs  metric.Float64Histogram
	InstrSandboxTotal   metric.Int64Counter

	instrOnce sync.Once
)

// ── ObservableGauge 的原子支撑值 ────────────────────────────────────────────

// ActiveAgentsCount 由外部调用 SetActiveAgents() 更新。
var ActiveAgentsCount atomic.Int64

// TaskSuccessCount / TaskTotalCount 由 RecordTaskOutcome() 更新。
var (
	TaskSuccessCount atomic.Int64
	TaskTotalCount   atomic.Int64
)

// ── InitMetrics ─────────────────────────────────────────────────────────────

// InitMetrics 注册所有业务指标 instrument。
// 仅在 otelMetricsHandler 的 otelOnce.Do 内部调用一次（Tier 1+）。
// Tier-0 legacy 路径不调用此函数，所有 Record* 函数在该路径下为静默 no-op。
func InitMetrics(meter metric.Meter) {
	instrOnce.Do(func() {
		initInstruments(meter)
		registerObservableGauges(meter)
	})
}

func initInstruments(meter metric.Meter) {
	// LLM 调用计数
	InstrLLMCallsTotal, _ = meter.Int64Counter(
		"polaris.llm.calls_total",
		metric.WithDescription("LLM 调用次数 (label: provider, model, status)"),
	)

	// LLM 延迟直方图（ExponentialBuckets 100ms→51.2s，M03 §2）
	InstrLLMLatencyMs, _ = meter.Float64Histogram(
		"polaris.llm.call_latency_ms",
		metric.WithDescription("LLM 调用端到端延迟（ms）(label: model)"),
		metric.WithExplicitBucketBoundaries(
			100, 200, 400, 800, 1600, 3200, 6400, 12800, 25600, 51200,
		),
	)

	// Token 消耗分类计数（input / output / cache_hit）
	InstrTokensTotal, _ = meter.Int64Counter(
		"polaris.tokens.consumed_total",
		metric.WithDescription("消耗 token 总数 (label: type: input/output/cache_hit)"),
	)

	// Cache Hit Rate Histogram (ISSUE-04)
	InstrLLMCacheHitRate, _ = meter.Float64Histogram(
		"polaris.llm.cache_hit_rate",
		metric.WithDescription("LLM Context Caching 命中率 (label: provider, model)"),
		metric.WithExplicitBucketBoundaries(0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0),
	)

	// API 费用（USD）
	InstrAPIcostUSD, _ = meter.Float64Counter(
		"polaris.api.cost_usd_total",
		metric.WithDescription("API 费用累计（USD）(label: provider, model, call_type)"),
	)

	// Stage3 FULLSTOP 边沿计数（与 M03 §3.2 KillSwitch 联动）
	InstrBurnStage3Total, _ = meter.Int64Counter(
		"polaris.token_burn.extreme_total",
		metric.WithDescription("TokenBurnRate Stage3 FULLSTOP 触发次数"),
	)
	InstrEventBufferDrainTimeoutTotal, _ = meter.Int64Counter(
		"polaris.eventbuffer.drain_timeout_total",
		metric.WithDescription("未写入 EventWriteBuffer 因 Stop 超时而丢弃的事件数"),
	)

	// 工具调用
	InstrToolCallsTotal, _ = meter.Int64Counter(
		"polaris.tool.calls_total",
		metric.WithDescription("工具调用次数 (label: tool_category, status, sandbox_tier)"),
	)

	InstrToolLatencyMs, _ = meter.Float64Histogram(
		"polaris.tool.call_latency_ms",
		metric.WithDescription("工具调用延迟（ms）(label: tool_category)"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 50, 100, 500, 1000, 5000),
	)

	// 沙箱执行次数（按 tier）
	InstrSandboxTotal, _ = meter.Int64Counter(
		"polaris.sandbox.executions_total",
		metric.WithDescription("沙箱执行次数 (label: tier: inprocess/l2/l3)"),
	)
}

func registerObservableGauges(meter metric.Meter) {
	goroutinesGauge, _ := meter.Float64ObservableGauge(
		"polaris.goroutines",
		metric.WithDescription("当前 goroutine 数量"),
	)
	memAllocMBGauge, _ := meter.Float64ObservableGauge(
		"polaris.memory_alloc_mb",
		metric.WithDescription("Go 堆已分配内存（MB）"),
	)
	agentsActiveGauge, _ := meter.Float64ObservableGauge(
		"polaris.agents_active",
		metric.WithDescription("当前活跃 Agent 数量"),
	)
	taskSuccessRateGauge, _ := meter.Float64ObservableGauge(
		"polaris.task_success_rate",
		metric.WithDescription("任务成功率（success/total，滑窗近似）"),
	)

	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		// goroutines & memory：直接从 runtime 读取，无额外 goroutine
		o.ObserveFloat64(goroutinesGauge, float64(runtime.NumGoroutine()))

		samples := []metrics.Sample{
			{Name: "/memory/classes/heap/objects:bytes"},
		}
		metrics.Read(samples)
		heapBytes := uint64(0)
		if samples[0].Value.Kind() == metrics.KindUint64 {
			heapBytes = samples[0].Value.Uint64()
		}
		o.ObserveFloat64(memAllocMBGauge, float64(heapBytes)/1024.0/1024.0)

		// agents active（外部通过 SetActiveAgents 更新）
		o.ObserveFloat64(agentsActiveGauge, float64(ActiveAgentsCount.Load()))

		// task success rate
		total := TaskTotalCount.Load()
		if total == 0 {
			o.ObserveFloat64(taskSuccessRateGauge, 1.0) // 冷启动默认 100%（无数据）
		} else {
			o.ObserveFloat64(taskSuccessRateGauge, float64(TaskSuccessCount.Load())/float64(total))
		}
		return nil
	}, goroutinesGauge, memAllocMBGauge, agentsActiveGauge, taskSuccessRateGauge)
}

// attribute helpers（内部使用，避免重复字面量）

func AttrProvider(v string) attribute.KeyValue    { return attribute.String("provider", v) }
func AttrModel(v string) attribute.KeyValue       { return attribute.String("model", v) }
func AttrStatus(v string) attribute.KeyValue      { return attribute.String("status", v) }
func AttrType(v string) attribute.KeyValue        { return attribute.String("type", v) }
func AttrCallType(v string) attribute.KeyValue    { return attribute.String("call_type", v) }
func AttrCategory(v string) attribute.KeyValue    { return attribute.String("tool_category", v) }
func AttrSandboxTier(v string) attribute.KeyValue { return attribute.String("sandbox_tier", v) }

// RecordLLMCacheHit 记录单次 LLM 调用的缓存命中情况。
// hit=true 表示本次调用命中了 Provider KV Cache（cache_read_input_tokens > 0）。
// 在各 Provider Adapter 的 Infer/StreamInfer 返回路径上调用。
func RecordLLMCacheHit(provider, model string, hit bool) {
	if InstrLLMCacheHitRate == nil {
		return
	}
	val := 0.0
	if hit {
		val = 1.0
	}
	InstrLLMCacheHitRate.Record(
		context.Background(),
		val,
		metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
		),
	)
}

// RecordMemoryToolCall 记录记忆工具调用指标。
// 在 InstrToolCallsTotal 为 nil 时静默跳过（Tier-0 无 OTel 场景）。
func RecordMemoryToolCall(ctx context.Context, toolName string, success bool) {
	if InstrToolCallsTotal == nil {
		return
	}
	result := "success"
	if !success {
		result = "failure"
	}
	InstrToolCallsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("tool", toolName),
			attribute.String("category", "memory"),
			attribute.String("result", result),
		),
	)
}
