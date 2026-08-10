package metrics

import (
	"log/slog"
	"sync"
	"sync/atomic"

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
	InstrOutboxProcessFailuresTotal    metric.Int64Counter // label: engine
	InstrOutboxCursorErrorsTotal       metric.Int64Counter // label: kind
	InstrMemoryJSONDecodeFailuresTotal metric.Int64Counter // label: table
	InstrBlackboardScanErrorsTotal     metric.Int64Counter // label: op

	// [GD-14-004 观测先行] HITL 审批频率与结果分布。
	// 只做观测，不参与任何放行决策——自适应降级（"同类申请批准过 N 次后
	// 自动放行"）是在削弱安全边界，必须先有真实频次/批准率数据支撑阈值，
	// 否则等于凭感觉打开一个越权口子。见 docs/arch/decisions ADR 待定项。
	InstrHITLPromptsTotal                  metric.Int64Counter // labels: checkpoint_type, agent_id
	InstrHITLDecisionsTotal                metric.Int64Counter // labels: checkpoint_type, decision, source
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

	// [阶段04 A-01] Agent Handoff 唤醒路径可观测性。path 固定枚举
	// "event"/"peek"/"poll_fallback"——生产上 poll_fallback 非零即代表
	// Blackboard 事件订阅链路有问题（GD-13-007）。
	InstrAgentHandoffWakeTotal metric.Int64Counter // label: path

	// [阶段04 A-02] Agent Handoff 崩溃续跑可观测性（GD-13-009）。
	// result 固定枚举 "restored"/"degraded"。
	InstrAgentHandoffResumeTotal            metric.Int64Counter // label: result
	InstrAgentHandoffSnapshotOversizedTotal metric.Int64Counter // 无 label（单一事件类型）

	// [GD-13-005] M9 Reflexion 反思并发信号量满时丢弃失败任务事件的计数。
	// engine.go handleTaskCompleteEvent 此前对该分支静默 return，被丢弃的失败
	// 任务既不进入 Reflexion，也不计入任何计数器——只有成功任务经
	// HeuristicsWriter.RecordSuccess 被观测到，长期运行下 success_rate 会系统性
	// 偏高（分母隐性丢了失败样本）。无 label（单一事件类型，不构成基数问题）。
	InstrLearningReflectionDroppedTotal metric.Int64Counter

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
