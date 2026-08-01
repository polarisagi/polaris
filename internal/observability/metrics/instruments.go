package metrics

import (
	"context"
	"log/slog"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ── 同步 instruments（Counter / Histogram）─────────────────────────────────
// 全部包级 nil 变量；InitMetrics 赋值后方可使用。
// RecordXxx 函数在 nil 时静默返回（Tier-0 legacy 路径安全）。

var (
	// M1 LLM 调用
	InstrLLMCallsTotal       metric.Int64Counter
	InstrLLMLatencyMs        metric.Float64Histogram
	InstrTokensTotal         metric.Int64Counter
	InstrAPIcostUSD          metric.Float64Counter
	InstrBurnStage3Total     metric.Int64Counter
	InstrLLMCacheHitRate     metric.Float64Histogram // (ISSUE-04)
	InstrGoroutinePanicTotal metric.Int64Counter
	InstrDbWriterPanicTotal  metric.Int64Counter

	// M7 工具调用 & 沙箱
	InstrToolCallsTotal metric.Int64Counter
	InstrToolLatencyMs  metric.Float64Histogram
	InstrSandboxTotal   metric.Int64Counter

	// [UP-01] Swarm Compensation
	InstrSwarmCompensationFailedTotal  metric.Int64Counter
	InstrSwarmCompensationTimeoutTotal metric.Int64Counter

	// System 1 Bypass
	InstrSystem1BypassTotal metric.Int64Counter

	// [Task 14] M10 Embedding 可观测性
	InstrEmbeddingLatencyMs  metric.Float64Histogram // embedding 调用延迟
	InstrEmbeddingErrorTotal metric.Int64Counter     // embedding 调用失败次数

	// [Task 14] M11 Cedar Policy 评估三态计数（allow/deny/degraded）
	InstrCedarAllowTotal    metric.Int64Counter // Cedar 评估结果: allow
	InstrCedarDenyTotal     metric.Int64Counter // Cedar 评估结果: deny
	InstrCedarDegradedTotal metric.Int64Counter // Cedar 评估降级（FFI 故障回退 Go 规则）

	// [Task 14] FFI 调用健康度（按 ffi_target 标签区分各 FFI 桥）
	InstrFFILatencyMs  metric.Float64Histogram // FFI 调用延迟
	InstrFFIErrorTotal metric.Int64Counter     // FFI 调用失败次数

	// [F-09] Retrieval Explain Bits
	InstrRetrievalExplainBitsTotal metric.Int64Counter

	// [GR-1-003] M10 Rerank 可观测性：RAG 链路中最耗时的重排步骤此前完全无埋点。
	InstrRerankLatencyMs  metric.Float64Histogram // Rerank 单次调用耗时（ms）
	InstrRerankCallsTotal metric.Int64Counter     // Rerank 调用计数 (label: outcome: success/fallback/timeout)

	// [UP-05] Shadow Executor
	InstrShadowReplayTotal  metric.Int64Counter
	InstrShadowSkippedTotal metric.Int64Counter
	InstrShadowDurationMs   metric.Float64Histogram
	InstrShadowPassRate     metric.Float64Histogram

	// [UP-06] Agent 流式事件广播：订阅者缓冲满导致的丢弃计数（HE-1 可观测）
	InstrAgentStreamDroppedTotal metric.Int64Counter

	// [阶段02-错误吞没整改] 带 label 的失败类指标，均为枚举有界值，无需 CardinalityGuard。
	InstrOutboxProcessFailuresTotal        metric.Int64Counter // label: engine
	InstrOutboxCursorErrorsTotal           metric.Int64Counter // label: kind
	InstrMemoryJSONDecodeFailuresTotal     metric.Int64Counter // label: table
	InstrBlackboardScanErrorsTotal         metric.Int64Counter // label: op
	InstrKnowledgeOutboxWriteFailuresTotal metric.Int64Counter // label: event_type
	InstrKnowledgeGraphWriteFailuresTotal  metric.Int64Counter // label: op
	InstrKnowledgeReadFailuresTotal        metric.Int64Counter // label: op（非 ErrNoRows 的真实读路径查询失败）
	InstrToolOutcomeDecodeFailuresTotal    metric.Int64Counter // label: tool_category（经 ToolCategory() 归一化，非原始 tool_name）
	InstrPIIMappingEvictionsTotal          metric.Int64Counter // 无 label（partitionKey 无界基数，不进维度，阶段03 R-02）
	InstrSandboxDowngradeTotal             metric.Int64Counter // label: from, to, reason（均为固定枚举值，有界基数，阶段03 R-05）

	// [阶段03 R-06] Skill/Plugin 生成器 LLM 结构化输出可观测性。kind 固定枚举
	// "skill"/"plugin"，result 固定枚举 "success"/"failure"/"circuit_open"。
	InstrExtensionLLMDurationMs              metric.Float64Histogram
	InstrExtensionLLMCallsTotal              metric.Int64Counter
	InstrExtensionLLMStructuredFailuresTotal metric.Int64Counter // label: kind

	instrOnce sync.Once
)

// ── ObservableGauge 的原子支撑值 ────────────────────────────────────────────

// ActiveAgentsCount 由外部调用 SetActiveAgents() 更新。
var ActiveAgentsCount atomic.Int64

// TaskSuccessCount / TaskTotalCount 由 RecordTaskOutcome() 更新。
var (
	TaskSuccessCount         atomic.Int64
	TaskTotalCount           atomic.Int64
	GlobalSkillCacheHitTotal atomic.Int64

	// GlobalSchemaValidationFailureTotal LLMFillEffect.SchemaRef 结构校验失败的累计次数
	// （internal/agent/schemavalidate，GR-4-005 复核修复）。持续增长说明 Prompt 模板与
	// LLM 实际产出的结构存在系统性偏差，或模型幻觉率异常，值得单独告警而非静默降级。
	GlobalSchemaValidationFailureTotal atomic.Int64
)

// ── InitMetrics ─────────────────────────────────────────────────────────────

// instrumentInitErrs 收集初始化期所有 OTel 同步 instrument（Counter/Histogram）
// 注册失败，避免 initInstruments 内 30+ 处逐个静默吞没（阶段02 §2.2）。
// attempts 记录总尝试次数（成功+失败），用于判定"是否全部失败"而无需硬编码总数。
type instrumentInitErrs struct {
	errs     []error
	attempts int
}

func (e *instrumentInitErrs) capture(name string, err error) {
	e.attempts++
	if err != nil {
		e.errs = append(e.errs, apperr.Wrap(apperr.CodeInternal, "instruments: register "+name, err))
	}
}

// metricsDegraded 标记 OTel instrument 注册是否发生部分失败。
// ADR-0001 豁免：observability 一等公民指标范畴，允许包级可变原子状态。
var metricsDegraded atomic.Bool

// InstrumentsDegraded 供 /healthz 与 FeatureGate 消费，暴露指标注册降级状态（HE-1，不静默）。
func InstrumentsDegraded() bool {
	return metricsDegraded.Load()
}

// evaluateInstrumentInitErrs 根据聚合结果判定是否降级 / 是否致命。
// 抽出为独立纯函数（不触碰 instrOnce/metricsDegraded 全局状态），便于脱离
// InitMetrics 的单例限制单独单元测试判定逻辑本身。
//
// 设计决策（阶段02 §2.2）：指标注册失败 = M3 可观测性大面积瘫痪，属初始化致命
// 错误，但不得 panic（2GB VPS 上偶发注册失败直接打死进程，违反 Tier-0 可用性
// 目标）。部分失败仅 Error 日志 + degraded=true，不阻断；仅当全部 instrument
// 都失败（说明 meter provider 根本没启动）才返回 fatal error。
func evaluateInstrumentInitErrs(ie *instrumentInitErrs) (degraded bool, fatal error) {
	if len(ie.errs) == 0 {
		return false, nil
	}
	slog.Error("observability: OTel instrument registration failed, metrics partially degraded",
		"failed_count", len(ie.errs), "total", ie.attempts, "first_err", ie.errs[0])
	degraded = true
	if len(ie.errs) >= ie.attempts {
		fatal = apperr.Wrap(apperr.CodeInternal,
			"InitMetrics: all sync instruments failed to register, meter provider likely not started",
			ie.errs[0])
	}
	return degraded, fatal
}

// InitMetrics 注册所有业务指标 instrument。
// 仅在 otelMetricsHandler 的 otelOnce.Do 内部调用一次（Tier 1+）。
// Tier-0 legacy 路径不调用此函数，所有 Record* 函数在该路径下为静默 no-op。
func InitMetrics(meter metric.Meter) error {
	var initErr error
	instrOnce.Do(func() {
		ie := &instrumentInitErrs{}
		initInstruments(meter, ie)
		registerObservableGauges(meter)

		degraded, fatal := evaluateInstrumentInitErrs(ie)
		if degraded {
			metricsDegraded.Store(true)
		}
		initErr = fatal
	})
	return initErr
}

func initInstruments(meter metric.Meter, ie *instrumentInitErrs) {
	var err error

	// LLM 调用计数
	InstrLLMCallsTotal, err = meter.Int64Counter(
		"polaris.llm.calls_total",
		metric.WithDescription("LLM 调用次数 (label: provider, model, status)"),
	)
	ie.capture("polaris.llm.calls_total", err)

	// LLM 延迟直方图（ExponentialBuckets 100ms→51.2s，M03 §2）
	InstrLLMLatencyMs, err = meter.Float64Histogram(
		"polaris.llm.call_latency_ms",
		metric.WithDescription("LLM 调用端到端延迟（ms）(label: model)"),
		metric.WithExplicitBucketBoundaries(
			100, 200, 400, 800, 1600, 3200, 6400, 12800, 25600, 51200,
		),
	)
	ie.capture("polaris.llm.call_latency_ms", err)

	// Token 消耗分类计数（input / output / cache_hit）
	InstrTokensTotal, err = meter.Int64Counter(
		"polaris.tokens.consumed_total",
		metric.WithDescription("消耗 token 总数 (label: type: input/output/cache_hit)"),
	)
	ie.capture("polaris.tokens.consumed_total", err)

	InstrSystem1BypassTotal, err = meter.Int64Counter(
		"polaris.system1_bypass_total",
		metric.WithDescription("System 1 Bypass 次数 (label: matched=true/false)"),
	)
	ie.capture("polaris.system1_bypass_total", err)

	InstrRetrievalExplainBitsTotal, err = meter.Int64Counter(
		"polaris.retrieval.explain_bits_total",
		metric.WithDescription("Retrieval explain bits distribution (label: bit)"),
	)
	ie.capture("polaris.retrieval.explain_bits_total", err)

	// Cache Hit Rate Histogram (ISSUE-04)
	InstrLLMCacheHitRate, err = meter.Float64Histogram(
		"polaris.llm.cache_hit_rate",
		metric.WithDescription("LLM Context Caching 命中率 (label: provider, model)"),
		metric.WithExplicitBucketBoundaries(0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0),
	)
	ie.capture("polaris.llm.cache_hit_rate", err)

	// API 费用（USD）
	InstrAPIcostUSD, err = meter.Float64Counter(
		"polaris.api.cost_usd_total",
		metric.WithDescription("API 费用累计（USD）(label: provider, model, call_type)"),
	)
	ie.capture("polaris.api.cost_usd_total", err)

	// Stage3 FULLSTOP 边沿计数（与 M03 §3.2 KillSwitch 联动）
	InstrBurnStage3Total, err = meter.Int64Counter(
		"polaris.token_burn.extreme_total",
		metric.WithDescription("TokenBurnRate Stage3 FULLSTOP 触发次数"),
	)
	ie.capture("polaris.token_burn.extreme_total", err)
	InstrGoroutinePanicTotal, err = meter.Int64Counter(
		"polaris.goroutine_panic_total",
		metric.WithDescription("SafeGo recover 的 panic 总数"),
	)
	ie.capture("polaris.goroutine_panic_total", err)
	InstrDbWriterPanicTotal, err = meter.Int64Counter(
		"polaris_dbwriter_panic",
		metric.WithDescription("DatabaseWriter Run panic 总数"),
	)
	ie.capture("polaris_dbwriter_panic", err)

	// 工具调用
	InstrToolCallsTotal, err = meter.Int64Counter(
		"polaris.tool.calls_total",
		metric.WithDescription("工具调用次数 (label: tool_category, status, sandbox_tier)"),
	)
	ie.capture("polaris.tool.calls_total", err)

	InstrToolLatencyMs, err = meter.Float64Histogram(
		"polaris.tool.call_latency_ms",
		metric.WithDescription("工具调用延迟（ms）(label: tool_category)"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 50, 100, 500, 1000, 5000),
	)
	ie.capture("polaris.tool.call_latency_ms", err)

	// 沙箱执行次数（按 tier）
	InstrSandboxTotal, err = meter.Int64Counter(
		"polaris.sandbox.executions_total",
		metric.WithDescription("沙箱执行次数 (label: tier: inprocess/l2/l3)"),
	)
	ie.capture("polaris.sandbox.executions_total", err)

	InstrSwarmCompensationFailedTotal, err = meter.Int64Counter(
		"polaris.swarm.compensation_failed_total",
		metric.WithDescription("Swarm compensation failed total (label: stage)"),
	)
	ie.capture("polaris.swarm.compensation_failed_total", err)

	InstrSwarmCompensationTimeoutTotal, err = meter.Int64Counter(
		"polaris.swarm.compensation_timeout_total",
		metric.WithDescription("Swarm compensation timeout total (label: stage)"),
	)
	ie.capture("polaris.swarm.compensation_timeout_total", err)

	// [UP-06] Agent 流式事件订阅者缓冲满丢弃计数
	InstrAgentStreamDroppedTotal, err = meter.Int64Counter(
		"polaris.agent.stream_dropped_total",
		metric.WithDescription("Agent 流式事件因订阅者缓冲满被丢弃的条数"),
	)
	ie.capture("polaris.agent.stream_dropped_total", err)

	// [Task 14] Embedding 指标：调用延迟 + 失败计数，便于排查 embedding 调用健康度。
	InstrEmbeddingLatencyMs, err = meter.Float64Histogram(
		"polaris.embedding.call_latency_ms",
		metric.WithDescription("Embedding 调用延迟（ms）(label: provider, model)"),
		metric.WithExplicitBucketBoundaries(5, 10, 25, 50, 100, 250, 500, 1000, 2000),
	)
	ie.capture("polaris.embedding.call_latency_ms", err)
	InstrEmbeddingErrorTotal, err = meter.Int64Counter(
		"polaris.embedding.errors_total",
		metric.WithDescription("Embedding 调用失败次数 (label: provider, model)"),
	)
	ie.capture("polaris.embedding.errors_total", err)

	// [Task 14] Cedar policy 评估三态：allow/deny/degraded，便于排查策略允许/拒绝比例。
	// InstrCedarDegradedTotal 已有旧名 GlobalCedarDegradedTotal，此处新增 allow/deny 两个。
	InstrCedarAllowTotal, err = meter.Int64Counter(
		"polaris.cedar.allow_total",
		metric.WithDescription("Cedar PolicyGate 评估结果: allow (label: action)"),
	)
	ie.capture("polaris.cedar.allow_total", err)
	InstrCedarDenyTotal, err = meter.Int64Counter(
		"polaris.cedar.deny_total",
		metric.WithDescription("Cedar PolicyGate 评估结果: deny (label: action, reason)"),
	)
	ie.capture("polaris.cedar.deny_total", err)
	InstrCedarDegradedTotal, err = meter.Int64Counter(
		"polaris.cedar.degraded_total",
		metric.WithDescription("Cedar FFI 故障降级为 Go 内置规则的次数"),
	)
	ie.capture("polaris.cedar.degraded_total", err)

	// [Task 14] FFI 调用健康度：延迟 + 失败计数，按 ffi_target 标签区分各 FFI 桥。
	InstrFFILatencyMs, err = meter.Float64Histogram(
		"polaris.ffi.call_latency_ms",
		metric.WithDescription("FFI 调用延迟（ms）(label: ffi_target: llama/cedar/surreal/sandbox)"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 5, 10, 50, 100, 500),
	)
	ie.capture("polaris.ffi.call_latency_ms", err)
	InstrFFIErrorTotal, err = meter.Int64Counter(
		"polaris.ffi.errors_total",
		metric.WithDescription("FFI 调用失败次数 (label: ffi_target)"),
	)
	ie.capture("polaris.ffi.errors_total", err)

	// [GR-1-003] Rerank 指标：调用延迟 + 结果计数，排查 RAG 重排步骤性能瓶颈与降级频率。
	InstrRerankLatencyMs, err = meter.Float64Histogram(
		"polaris.rerank.call_latency_ms",
		metric.WithDescription("Reranker 单次调用延迟（ms）(label: outcome)"),
		metric.WithExplicitBucketBoundaries(5, 10, 25, 50, 100, 250, 500, 1000, 2000),
	)
	ie.capture("polaris.rerank.call_latency_ms", err)
	InstrRerankCallsTotal, err = meter.Int64Counter(
		"polaris.rerank.calls_total",
		metric.WithDescription("Reranker 调用计数 (label: outcome: success/fallback/timeout)"),
	)
	ie.capture("polaris.rerank.calls_total", err)

	InstrShadowReplayTotal, err = meter.Int64Counter(
		"polaris.shadow.replay_total",
		metric.WithDescription("Shadow replay batches total"),
	)
	ie.capture("polaris.shadow.replay_total", err)
	InstrShadowSkippedTotal, err = meter.Int64Counter(
		"polaris.shadow.skipped_total",
		metric.WithDescription("Shadow skipped samples total"),
	)
	ie.capture("polaris.shadow.skipped_total", err)
	InstrShadowDurationMs, err = meter.Float64Histogram(
		"polaris.shadow.duration_ms",
		metric.WithDescription("Shadow replay batch duration ms"),
		metric.WithExplicitBucketBoundaries(10, 50, 100, 500, 1000, 5000, 10000, 30000),
	)
	ie.capture("polaris.shadow.duration_ms", err)
	InstrShadowPassRate, err = meter.Float64Histogram(
		"polaris.shadow.pass_rate",
		metric.WithDescription("Shadow replay pass rate"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 0.8, 0.9, 0.95, 0.99, 1.0),
	)
	ie.capture("polaris.shadow.pass_rate", err)

	// [阶段02-错误吞没整改] §4 新增指标，详见 local_playground/upgrade/02-error-handling.md
	InstrOutboxProcessFailuresTotal, err = meter.Int64Counter(
		"polaris.outbox.process_failures_total",
		metric.WithDescription("单条 outbox 记录处理失败次数 (label: engine)"),
	)
	ie.capture("polaris.outbox.process_failures_total", err)
	InstrOutboxCursorErrorsTotal, err = meter.Int64Counter(
		"polaris.outbox.cursor_errors_total",
		metric.WithDescription("outbox 游标加载/持久化失败次数 (label: kind: load/save)"),
	)
	ie.capture("polaris.outbox.cursor_errors_total", err)
	InstrMemoryJSONDecodeFailuresTotal, err = meter.Int64Counter(
		"polaris.memory.json_decode_failures_total",
		metric.WithDescription("记忆子系统 JSON/Scan 反序列化失败次数 (label: table)"),
	)
	ie.capture("polaris.memory.json_decode_failures_total", err)
	InstrBlackboardScanErrorsTotal, err = meter.Int64Counter(
		"polaris.blackboard.scan_errors_total",
		metric.WithDescription("Blackboard 行扫描/查询失败次数 (label: op)"),
	)
	ie.capture("polaris.blackboard.scan_errors_total", err)
	InstrKnowledgeOutboxWriteFailuresTotal, err = meter.Int64Counter(
		"polaris.knowledge.outbox_write_failures_total",
		metric.WithDescription("知识管线 outbox 事件投递失败次数 (label: event_type)"),
	)
	ie.capture("polaris.knowledge.outbox_write_failures_total", err)
	InstrKnowledgeGraphWriteFailuresTotal, err = meter.Int64Counter(
		"polaris.knowledge.graph_write_failures_total",
		metric.WithDescription("GraphRAG 实体/边落库失败次数 (label: op)"),
	)
	ie.capture("polaris.knowledge.graph_write_failures_total", err)
	InstrKnowledgeReadFailuresTotal, err = meter.Int64Counter(
		"polaris.knowledge.read_failures_total",
		metric.WithDescription("知识管线读路径查询失败次数，非 ErrNoRows (label: op)"),
	)
	ie.capture("polaris.knowledge.read_failures_total", err)
	InstrToolOutcomeDecodeFailuresTotal, err = meter.Int64Counter(
		"polaris.tool.outcome_decode_failures_total",
		metric.WithDescription("工具 outcome JSON 解析失败次数 (label: tool_category)"),
	)
	ie.capture("polaris.tool.outcome_decode_failures_total", err)
	InstrPIIMappingEvictionsTotal, err = meter.Int64Counter(
		"polaris.pii.mapping_evictions_total",
		metric.WithDescription("PIIDesensitizer 分区内映射 LRU 淘汰次数（无 label，阶段03 R-02）"),
	)
	ie.capture("polaris.pii.mapping_evictions_total", err)
	InstrSandboxDowngradeTotal, err = meter.Int64Counter(
		"polaris.sandbox.downgrade_total",
		metric.WithDescription("沙箱隔离层级降级次数 (label: from, to, reason，均为固定枚举值)"),
	)
	ie.capture("polaris.sandbox.downgrade_total", err)
	InstrExtensionLLMDurationMs, err = meter.Float64Histogram(
		"polaris.extension.llm_duration_ms",
		metric.WithDescription("Skill/Plugin 生成器结构化 LLM 调用端到端延迟，含重试（ms）(label: kind)"),
		metric.WithExplicitBucketBoundaries(
			100, 200, 400, 800, 1600, 3200, 6400, 12800, 25600, 51200,
		),
	)
	ie.capture("polaris.extension.llm_duration_ms", err)
	InstrExtensionLLMCallsTotal, err = meter.Int64Counter(
		"polaris.extension.llm_calls_total",
		metric.WithDescription("Skill/Plugin 生成器调用次数 (label: kind, result: success/failure/circuit_open)"),
	)
	ie.capture("polaris.extension.llm_calls_total", err)
	InstrExtensionLLMStructuredFailuresTotal, err = meter.Int64Counter(
		"polaris.extension.llm_structured_failures_total",
		metric.WithDescription("Skill/Plugin 生成器结构化 JSON 解析重试耗尽次数 (label: kind)"),
	)
	ie.capture("polaris.extension.llm_structured_failures_total", err)
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

// RecordEmbeddingCall 记录一次 embedding 调用的延迟与失败情况。
// 2026-07-04 审计修复（Task 14）：InstrEmbeddingLatencyMs/InstrEmbeddingErrorTotal
// 此前已定义但从未被任何调用方记录，embedding 调用健康度完全不可观测；
// 现接入 internal/llm/adapter/embedding.go 的两个真实 HTTP 调用路径。
// InstrEmbeddingLatencyMs 为 nil 时静默跳过（Tier-0 无 OTel 场景）。
func RecordEmbeddingCall(ctx context.Context, provider, model string, latencyMs float64, err error) {
	if InstrEmbeddingLatencyMs != nil {
		InstrEmbeddingLatencyMs.Record(ctx, latencyMs,
			metric.WithAttributes(AttrProvider(provider), AttrModel(model)))
	}
	if err != nil && InstrEmbeddingErrorTotal != nil {
		InstrEmbeddingErrorTotal.Add(ctx, 1,
			metric.WithAttributes(AttrProvider(provider), AttrModel(model)))
	}
}

// RecordRerankCall 记录一次 Reranker 调用的延迟与结果分类。
// outcome 建议取值：success（正常完成重排）/ fallback（超时或 panic 后降级为透传原始
// 顺序，见 search.SafeRerank）/ timeout（专指超时降级，若调用方能区分超时与 panic 可细化）。
// InstrRerankLatencyMs 为 nil 时静默跳过（Tier-0 无 OTel 场景）。
func RecordRerankCall(ctx context.Context, outcome string, latencyMs float64) {
	if InstrRerankLatencyMs != nil {
		InstrRerankLatencyMs.Record(ctx, latencyMs, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	if InstrRerankCallsTotal != nil {
		InstrRerankCallsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// RecordGoroutinePanic 记录 SafeGo/panic。
func RecordGoroutinePanic() {
	if InstrGoroutinePanicTotal != nil {
		InstrGoroutinePanicTotal.Add(context.Background(), 1)
	}
}

// RecordDbWriterPanic 记录 DatabaseWriter panic。
func RecordDbWriterPanic() {
	if InstrDbWriterPanicTotal != nil {
		InstrDbWriterPanicTotal.Add(context.Background(), 1)
	}
}

// RecordFFICall 记录一次 FFI 桥调用的延迟与失败情况。
// 2026-07-04 审计修复（Task 14）：InstrFFILatencyMs/InstrFFIErrorTotal 此前已定义
// 但从未被任何调用方记录；现接入 internal/ffi/llama.go 的本地推理 FFI 调用路径。
// target 建议取值：llama/cedar/surreal/sandbox（与指标描述中的 label 约定一致）。
func RecordFFICall(ctx context.Context, target string, latencyMs float64, err error) {
	if InstrFFILatencyMs != nil {
		InstrFFILatencyMs.Record(ctx, latencyMs,
			metric.WithAttributes(attribute.String("ffi_target", target)))
	}
	if err != nil && InstrFFIErrorTotal != nil {
		InstrFFIErrorTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("ffi_target", target)))
	}
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

func RecordExplainBit(ctx context.Context, bit string) {
	if InstrRetrievalExplainBitsTotal != nil {
		InstrRetrievalExplainBitsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("bit", bit)))
	}
}

// ── [阶段02-错误吞没整改] Record 辅助函数 ────────────────────────────────────
// 均为 nil-safe（Tier-0 legacy 路径无 OTel meter 时静默 no-op）。

// RecordOutboxProcessFailure 记录单条 outbox 记录处理失败（L2，不中断批次）。
func RecordOutboxProcessFailure(ctx context.Context, engine string) {
	if InstrOutboxProcessFailuresTotal != nil {
		InstrOutboxProcessFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("engine", engine)))
	}
}

// RecordOutboxCursorError 记录 outbox 游标加载/持久化失败，kind ∈ {load, save}。
func RecordOutboxCursorError(ctx context.Context, kind string) {
	if InstrOutboxCursorErrorsTotal != nil {
		InstrOutboxCursorErrorsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
	}
}

// RecordMemoryJSONDecodeFailure 记录记忆子系统字段反序列化失败（L3，字段保持零值）。
func RecordMemoryJSONDecodeFailure(ctx context.Context, table string) {
	if InstrMemoryJSONDecodeFailuresTotal != nil {
		InstrMemoryJSONDecodeFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("table", table)))
	}
}

// RecordBlackboardScanError 记录 Blackboard 行扫描/查询失败，op 为调用点标识。
func RecordBlackboardScanError(ctx context.Context, op string) {
	if InstrBlackboardScanErrorsTotal != nil {
		InstrBlackboardScanErrorsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op)))
	}
}

// RecordKnowledgeOutboxWriteFailure 记录知识管线 outbox 事件投递失败。
func RecordKnowledgeOutboxWriteFailure(ctx context.Context, eventType string) {
	if InstrKnowledgeOutboxWriteFailuresTotal != nil {
		InstrKnowledgeOutboxWriteFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", eventType)))
	}
}

// RecordKnowledgeGraphWriteFailure 记录 GraphRAG 实体/边落库失败。
func RecordKnowledgeGraphWriteFailure(ctx context.Context, op string) {
	if InstrKnowledgeGraphWriteFailuresTotal != nil {
		InstrKnowledgeGraphWriteFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op)))
	}
}

// RecordKnowledgeReadFailure 记录知识管线读路径真实查询失败（非 ErrNoRows）。
func RecordKnowledgeReadFailure(ctx context.Context, op string) {
	if InstrKnowledgeReadFailuresTotal != nil {
		InstrKnowledgeReadFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op)))
	}
}

// RecordToolOutcomeDecodeFailure 记录工具 outcome JSON 解析失败。
// toolName 经 ToolCategory() 归一化为 mcp/skill/builtin 后才作为 label，原始 tool_name 只进日志（禁止高基 label）。
func RecordToolOutcomeDecodeFailure(ctx context.Context, toolName string) {
	if InstrToolOutcomeDecodeFailuresTotal != nil {
		InstrToolOutcomeDecodeFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("tool_category", ToolCategory(toolName))))
	}
}

// RecordPIIMappingEviction 记录一次 PIIDesensitizer 分区内映射 LRU 淘汰。
// 不带 label（分区键=SessionID 通常无界基数，禁止进指标维度，阶段03 R-02）。
func RecordPIIMappingEviction() {
	if InstrPIIMappingEvictionsTotal != nil {
		InstrPIIMappingEvictionsTotal.Add(context.Background(), 1)
	}
}

// RecordSandboxDowngrade 记录一次沙箱隔离层级降级（阶段03 R-05）。
// from/to/reason 必须是调用方硬编码的固定枚举字符串（如 "wasm"/"inprocess"/
// "trusted_source_opt_in"），禁止传入任何用户/工具可控值（CardinalityGuard）。
func RecordSandboxDowngrade(ctx context.Context, from, to, reason string) {
	if InstrSandboxDowngradeTotal != nil {
		InstrSandboxDowngradeTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("from", from),
			attribute.String("to", to),
			attribute.String("reason", reason),
		))
	}
}

// RecordExtensionLLMCall 记录一次 Skill/Plugin 生成器的结构化 LLM 生成调用
// （阶段03 R-06）。kind 固定枚举 "skill"/"plugin"；result 固定枚举
// "success"/"failure"/"circuit_open"；durationMs 覆盖含重试的完整耗时。
func RecordExtensionLLMCall(ctx context.Context, kind, result string, durationMs float64) {
	if InstrExtensionLLMCallsTotal != nil {
		InstrExtensionLLMCallsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("kind", kind),
			attribute.String("result", result),
		))
	}
	if InstrExtensionLLMDurationMs != nil {
		InstrExtensionLLMDurationMs.Record(ctx, durationMs, metric.WithAttributes(attribute.String("kind", kind)))
	}
}

// RecordExtensionStructuredFailure 记录一次结构化 JSON 重试耗尽失败（阶段03 R-06）。
func RecordExtensionStructuredFailure(ctx context.Context, kind string) {
	if InstrExtensionLLMStructuredFailuresTotal != nil {
		InstrExtensionLLMStructuredFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
	}
}
