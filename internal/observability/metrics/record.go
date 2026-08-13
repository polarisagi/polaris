package metrics

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// 指标记录函数与 attribute 辅助（R7 拆分自 instruments.go）。
// instrument 声明见 instruments.go；初始化见 instruments_init.go。

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

// RecordHITLPrompt 记录一次 HITL 审批发起（GD-14-004 观测先行）。
//
// agentID 基数说明：Agent ID 形如 "agent-{sessionID}"，理论上无界。这里刻意
// **不**直接打 agentID，而是由调用方传入已归一化的值（当前传空串或固定角色名），
// 避免 Prometheus 时间序列爆炸。真正需要按 Agent 下钻时走 decision_log/审计表，
// 不走指标维度（与 InstrPIIMappingEvictionsTotal 不打 partitionKey 同一原则）。
func RecordHITLPrompt(ctx context.Context, checkpointType, agentID string) {
	if InstrHITLPromptsTotal != nil {
		InstrHITLPromptsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("checkpoint_type", checkpointType),
			attribute.String("agent_id", agentID),
		))
	}
}

// RecordHITLDecision 记录一次 HITL 审批结果（GD-14-004 观测先行）。
// decision 取 "approved"/"denied"；source 取 "human"/"auto_approve"/"auto_deny"/
// "timeout_kill_pause"/"auto_denied_p0_regression"，均为固定枚举，基数有界。
func RecordHITLDecision(ctx context.Context, checkpointType, decision, source string) {
	if InstrHITLDecisionsTotal != nil {
		InstrHITLDecisionsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("checkpoint_type", checkpointType),
			attribute.String("decision", decision),
			attribute.String("source", source),
		))
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

// RecordAgentHandoffWake 记录一次 Agent handoff 唤醒（阶段04 A-01，GD-13-007）。
// path 必须是调用方硬编码的固定枚举字符串（"event"/"peek"/"poll_fallback"），
// 禁止传入任何用户/任务可控值（CardinalityGuard）。
func RecordAgentHandoffWake(ctx context.Context, path string) {
	if InstrAgentHandoffWakeTotal != nil {
		InstrAgentHandoffWakeTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("path", path)))
	}
}

// RecordAgentHandoffResume 记录一次 Agent handoff 崩溃续跑结果（阶段04 A-02，
// GD-13-009）。result 必须是调用方硬编码的固定枚举字符串（"restored"/
// "degraded"），禁止传入任何用户/任务可控值（CardinalityGuard）。
func RecordAgentHandoffResume(ctx context.Context, result string) {
	if InstrAgentHandoffResumeTotal != nil {
		InstrAgentHandoffResumeTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	}
}

// RecordAgentHandoffSnapshotOversized 记录一次 handoff 恢复快照因超出体积
// 上限而放弃落盘（阶段04 A-02）。无 label：单一事件类型，无需维度。
func RecordAgentHandoffSnapshotOversized(ctx context.Context) {
	if InstrAgentHandoffSnapshotOversizedTotal != nil {
		InstrAgentHandoffSnapshotOversizedTotal.Add(ctx, 1)
	}
}

// RecordRetrievalLatency 记录检索各阶段延迟（D-2）
func RecordRetrievalLatency(ctx context.Context, stage string, ms int64) {
	if InstrRetrievalLatencyMs != nil {
		InstrRetrievalLatencyMs.Record(ctx, float64(ms), metric.WithAttributes(attribute.String("stage", stage)))
	}
}

// RecordRetrievalRouteTimeout 记录混合检索单路超时降级（HE-1：能算就要上报）。
//
// 必须真正落到 Instrument 上：只打日志不打点的 Record* 函数比没有更糟——
// 它在 metrics 包里、名字叫 Record，读代码的人会以为这条降级已经可观测，
// 于是不会再去补埋点，而面板上永远看不到"向量路一直在静默降级"。
func RecordRetrievalRouteTimeout(ctx context.Context, route string) {
	slog.WarnContext(ctx, "retrieval route timeout", "route", route)
	if InstrRetrievalRouteTimeouts != nil {
		InstrRetrievalRouteTimeouts.Add(ctx, 1, metric.WithAttributes(attribute.String("route", route)))
	}
}

// RecordDownloaderResumeRestart 记录跨源续传因内容同一性校验不通过而清零重下。
//
// 埋点落在本包而不是 internal/downloader：ADR-0001 把「一等公民指标」的包级
// 变量豁免限定在 observability/metrics，downloader 自建 promauto 计数器会直接
// 触发 inv_NoGlobalVar（本轮实测已触发）。
func RecordDownloaderResumeRestart(ctx context.Context) {
	if InstrDownloaderResumeRestarts != nil {
		InstrDownloaderResumeRestarts.Add(ctx, 1)
	}
}
