package metrics

// 2026-08-02 拆分（Test_inv_FileLineLimit R7 400 行上限存量债务，见
// local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现）：
// TokenBurnRate 迁至 metrics_tokenburn.go，SurpriseIndex 迁至
// metrics_surprise.go，纯搬运无行为变更。本文件保留包级全局指标变量与
// 创始锚点漂移评分委托。

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	// GlobalSurpriseIndex 全局 SurpriseIndex 单例，sync.OnceValue 惰性初始化。
	// 调用方用 GlobalSurpriseIndex() 而非 GlobalSurpriseIndex 直接访问实例。
	GlobalSurpriseIndex = sync.OnceValue(func() *SurpriseIndex { return NewSurpriseIndex() })

	// GlobalKillswitchStage 全局 KillSwitch 阶段原子量（0=Normal…3=FullStop）。
	// killswitch.go 的 StateChangeCallback 写入；M13 handleStatus 读取。
	GlobalKillswitchStage atomic.Int32

	// GlobalCedarDegradedTotal tracks the number of times Cedar FFI evaluation failed.
	GlobalCedarDegradedTotal atomic.Int64

	// GlobalCedarFFILeaksTotal 累计 Cedar FFI goroutine 泄漏数（只增不减）。
	// 阶段03 R-01：policy.Gate 的 KillSwitch 判定改用滑动窗口计数，本变量仅
	// 用于长期趋势观测（polaris_cedar_ffi_leaks_total），不参与 KillSwitch 判定。
	GlobalCedarFFILeaksTotal atomic.Int64

	// GlobalOutboxDeadLetterTotal tracks the number of outbox records marked as dead.
	GlobalOutboxDeadLetterTotal atomic.Int64

	// GlobalFactualityJudgeUnavailableTotal 记录 L3 SemanticJudge 因超时/故障静默降级的累计次数。
	// factuality_guard.go semanticJudge() 在 llm_judge_unavailable 时写入。
	// OTel gauge 在 RegisterMetrics() 的 RegisterCallback 中注册。
	GlobalFactualityJudgeUnavailableTotal atomic.Int64

	// GlobalBlindZoneRoutingTotal 因 BlindZone 检测强制升级为 System2 的累计次数（V8-S4）。
	GlobalBlindZoneRoutingTotal atomic.Int64

	// GlobalReplanExtActivationDegradedTotal S_REPLAN 阶段扩展激活重试耗尽后降级的累计次数（A-3）。
	GlobalReplanExtActivationDegradedTotal atomic.Int64

	// GlobalTraceExporterErrorsTotal SpanExporter.ExportSpan 失败的累计次数（ADR-0069）。
	// trace.Tracer.EndSpan 异步导出失败时写入；导出是尽力而为语义，失败不影响主链路，
	// 仅通过该指标供运维观测导出健康度。
	GlobalTraceExporterErrorsTotal atomic.Int64

	// ── [阶段02-错误吞没整改] 无 label 计数器，语义单一无需按维度拆分 ──────────────
	// GlobalMemorySupersedeFailuresTotal 语义超越（MarkEntitySuperseded）失败累计次数，
	// 失败意味着旧信念与新信念并存（事实层数据损坏）。
	GlobalMemorySupersedeFailuresTotal atomic.Int64
	// GlobalMemoryEvictEventLostTotal 工作记忆驱逐事件归档失败累计次数（影响审计链重建）。
	GlobalMemoryEvictEventLostTotal atomic.Int64
	// GlobalMemoryFTSIndexFailuresTotal 情景记忆 FTS 索引写入失败累计次数（持续性能退化）。
	GlobalMemoryFTSIndexFailuresTotal atomic.Int64
	// GlobalMemoryColdArchiveDetachFailuresTotal EventArchiver DETACH DATABASE 失败累计次数
	// （defer 内尽力而为，失败会累积连接句柄，值得单独观测）。
	GlobalMemoryColdArchiveDetachFailuresTotal atomic.Int64
	// GlobalBlackboardFailTaskErrorsTotal DebateWorker.FailTask 失败累计次数（任务卡在既非成功也非失败态，依赖 Reaper 兜底）。
	GlobalBlackboardFailTaskErrorsTotal atomic.Int64
	// GlobalLearningCursorErrorsTotal 自进化引擎游标扫描失败累计次数（会导致从错误位置重放）。
	GlobalLearningCursorErrorsTotal atomic.Int64
	// GlobalLearningSkillRegisterFailuresTotal 合成技能注册失败累计次数（LLM 调用成本已花掉但成果丢失）。
	GlobalLearningSkillRegisterFailuresTotal atomic.Int64
	// GlobalGatewayPreferenceWriteFailuresTotal Gateway 偏好/系统提示词模板落库失败累计次数。
	GlobalGatewayPreferenceWriteFailuresTotal atomic.Int64
	// GlobalCronNextRunWriteFailuresTotal cron_create 工具创建任务后回填 next_run_at
	// 失败的累计次数（调度时间可能错乱，不影响任务本身已创建成功）。
	GlobalCronNextRunWriteFailuresTotal atomic.Int64
	// GlobalSchemaMigrationDiagWriteFailuresTotal SchemaManager.BeginMigration 写入
	// migration_version 诊断字段失败累计次数（不影响 migration_status 主状态机正确性，
	// 仅影响崩溃后人工排查时"上次卡在哪个版本"的可诊断性，定级 L2）。
	GlobalSchemaMigrationDiagWriteFailuresTotal atomic.Int64
	// GlobalCheckpointWriteFailuresTotal DebateExecutor/StateGraphExecutor 写入
	// task_checkpoints 失败累计次数（2026-08-02 补齐，见
	// local_playground/upgrade/99-new-findings.md 阶段02 §2.5 发现）。影响崩溃恢复
	// 续跑的可诊断性——checkpoint 写入是尽力而为（不阻断主执行链路），但持续失败
	// 意味着崩溃后无法从最近状态续跑，值得单独观测，定级 L2。
	GlobalCheckpointWriteFailuresTotal atomic.Int64
)

// foundingAnchorDriftScorePtr 以 atomic.Pointer 持有漂移评分注入函数，避免包级可变 var。
// atomic.Pointer[T] 零值为 nil，Load() 安全返回 nil，无需 init()。
var foundingAnchorDriftScorePtr atomic.Pointer[func() float64]

// SetFoundingAnchorDriftScorer 供 main.go 在启动时注入真实漂移评分实现（避免包循环依赖）。
func SetFoundingAnchorDriftScorer(fn func() float64) {
	foundingAnchorDriftScorePtr.Store(&fn)
}

// GetFoundingAnchorDriftScore 返回创始锚点漂移评分（委托给注入函数）。
// 若实现未注入（测试/冷启动场景），安全返回 0.0。
func GetFoundingAnchorDriftScore() float64 {
	if p := foundingAnchorDriftScorePtr.Load(); p != nil {
		return (*p)()
	}
	return 0.0
}

// RecordSystem1Bypass 记录 System-1 Bypass 触发次数。
func RecordSystem1Bypass(ctx context.Context, matched bool) {
	if InstrSystem1BypassTotal != nil {
		matchedStr := "false"
		if matched {
			matchedStr = "true"
		}
		InstrSystem1BypassTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("matched", matchedStr)))
	}
}
