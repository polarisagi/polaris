package metrics

import (
	"context"
	"runtime"
	"runtime/metrics"

	"go.opentelemetry.io/otel/metric"
)

// instrument 初始化与可观测 Gauge 注册（R7 拆分自 instruments.go）。
// 声明见 instruments.go；记录函数见 record.go。

// InitMetrics 注册所有业务指标 instrument。
// 仅在 otelMetricsHandler 的 otelOnce.Do 内部调用一次（Tier 1+）。
// Tier-0 legacy 路径不调用此函数，所有 Record* 函数在该路径下为静默 no-op。
func InitMetrics(meter metric.Meter) error {
	var initErr error
	instrOnce.Do(func() {
		ie := &instrumentInitErrs{}
		initInstruments(meter, ie)
		registerObservableGauges(meter, ie)

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
	InstrRetrievalLatencyMs, err = meter.Float64Histogram(
		"polaris.retrieval.latency_ms",
		metric.WithDescription("Retrieval execution latency in ms (label: stage)"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000),
	)
	ie.capture("polaris.retrieval.latency_ms", err)

	initDegradationInstruments(meter, ie)

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
	InstrHITLPromptsTotal, err = meter.Int64Counter(
		"polaris.hitl.prompts_total",
		metric.WithDescription("HITL 审批发起次数 (labels: checkpoint_type, agent_id)"),
	)
	ie.capture("polaris.hitl.prompts_total", err)
	InstrHITLDecisionsTotal, err = meter.Int64Counter(
		"polaris.hitl.decisions_total",
		metric.WithDescription("HITL 审批结果分布 (labels: checkpoint_type, decision, source)"),
	)
	ie.capture("polaris.hitl.decisions_total", err)
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
	InstrAgentHandoffWakeTotal, err = meter.Int64Counter(
		"polaris.agent.handoff_wake_total",
		metric.WithDescription("Agent handoff 唤醒路径计数 (label: path，取值 event/peek/poll_fallback)"),
	)
	ie.capture("polaris.agent.handoff_wake_total", err)
	InstrAgentHandoffResumeTotal, err = meter.Int64Counter(
		"polaris.agent.handoff_resume_total",
		metric.WithDescription("Agent handoff 崩溃续跑结果计数 (label: result，取值 restored/degraded)"),
	)
	ie.capture("polaris.agent.handoff_resume_total", err)
	InstrAgentHandoffSnapshotOversizedTotal, err = meter.Int64Counter(
		"polaris.agent.handoff_snapshot_oversized_total",
		metric.WithDescription("Agent handoff 恢复快照超出体积上限、放弃落盘的次数（无 label）"),
	)
	ie.capture("polaris.agent.handoff_snapshot_oversized_total", err)

	// [GD-13-005] M9 Reflexion 反思信号量满导致失败任务事件被丢弃的计数。
	InstrLearningReflectionDroppedTotal, err = meter.Int64Counter(
		"polaris.learning.reflection_dropped_total",
		metric.WithDescription("M9 Reflexion 反思并发信号量满时丢弃的失败任务事件数（无 label）"),
	)
	ie.capture("polaris.learning.reflection_dropped_total", err)
}

func registerObservableGauges(meter metric.Meter, ie *instrumentInitErrs) {
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

	// [2026-08-02 HE-1 补齐] RegisterCallback 失败此前静默丢弃（_, _ =），意味着
	// 本组 ObservableGauge（goroutines/内存/活跃Agent数/任务成功率）会在 /metrics
	// 上永久缺失且无任何日志线索——运维排查"某个 gauge 消失了"时无处下手。
	// 现改为经 instrumentInitErrs 聚合（与 initInstruments 的同步 instrument 走
	// 同一条 degraded 判定路径，见 evaluateInstrumentInitErrs），见
	// local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现。
	_, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
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
	ie.capture("observable_gauges_callback", err)
}
