package metrics

import (
	"strings"
	"sync/atomic"
)

// counterSpec 描述一个无 label 的 Global*Total 累计计数器在 /metrics 上的暴露方式。
//
// 为什么表驱动（GR-1.2-004）：此前 OTel 与 legacy 两个 handler 各自手写一份注册/输出清单，
// 新增计数器只要漏改其中一处就成了"能算不上报"的孤岛（HE-1）——实测漏了 8 个
// （检查点写失败、技能缓存命中、重规划降级、Trace 导出错误、4 个 updater 签名计数）。
// 单一清单 + TestSimpleCounters_CoverAllGlobalTotals 反射校验，使遗漏在 go test 阶段即报红。
type counterSpec struct {
	name string // OTel 名（点分）；legacy 名由 promName() 派生，与 Prometheus exporter 转换规则一致
	help string
	v    *atomic.Int64
}

func (c counterSpec) promName() string { return strings.ReplaceAll(c.name, ".", "_") }

// simpleCounters 返回全部无 label 累计计数器（每次调用构造新切片，非可变全局状态）。
func simpleCounters() []counterSpec {
	return []counterSpec{
		{"polaris.cedar.degraded_total", "Total number of Cedar FFI evaluation failures", &GlobalCedarDegradedTotal},
		{"polaris.cedar.ffi_leaks_total", "Cumulative count of Cedar FFI goroutine leaks (timeout-triggered)", &GlobalCedarFFILeaksTotal},
		{"polaris.outbox.dead_letter_total", "Total number of outbox messages dead", &GlobalOutboxDeadLetterTotal},
		{"polaris.factuality.judge_unavailable_total", "Factuality judge unavailable count", &GlobalFactualityJudgeUnavailableTotal},
		{"polaris.blind_zone.routing_total", "Forced System2 escalations due to BlindZone detection", &GlobalBlindZoneRoutingTotal},
		{"polaris.agent.schema_validation_failure_total", "LLMFillEffect responses failing SchemaRef validation", &GlobalSchemaValidationFailureTotal},
		{"polaris.agent.skill_cache_hit_total", "Skill cache hits in effect execution", &GlobalSkillCacheHitTotal},
		{"polaris.agent.replan_ext_activation_degraded_total", "S_REPLAN extension activation degraded after retries exhausted", &GlobalReplanExtActivationDegradedTotal},
		{"polaris.orchestrator.checkpoint_write_failures_total", "Debate/StateGraph checkpoint write failures", &GlobalCheckpointWriteFailuresTotal},
		{"polaris.trace.exporter_errors_total", "SpanExporter.ExportSpan failures", &GlobalTraceExporterErrorsTotal},
		{"polaris.updater.weak_trust_verify_total", "Update verifications completed in weak-trust mode", &GlobalUpdaterWeakTrustVerifyTotal},
		{"polaris.updater.signing_not_provisioned_total", "Update verifications without an embedded trust root", &GlobalUpdaterSigningNotProvisionedTotal},
		{"polaris.updater.signature_verified_total", "Update checksum signatures verified", &GlobalUpdaterSignatureVerifiedTotal},
		{"polaris.updater.signature_rejected_total", "Updates rejected for missing or invalid signature", &GlobalUpdaterSignatureRejectedTotal},
		{"polaris.memory.persistence_failures_total", "Episodic writes that hit storage-unavailable and suspended the FSM", &GlobalMemoryPersistenceFailuresTotal},
		{"polaris.memory.supersede_failures_total", "Semantic supersede marking failures", &GlobalMemorySupersedeFailuresTotal},
		{"polaris.memory.evict_event_lost_total", "Working memory eviction event archive failures", &GlobalMemoryEvictEventLostTotal},
		{"polaris.memory.fts_index_failures_total", "Episodic memory FTS index write failures", &GlobalMemoryFTSIndexFailuresTotal},
		{"polaris.memory.cold_archive_detach_failures_total", "EventArchiver DETACH DATABASE failures", &GlobalMemoryColdArchiveDetachFailuresTotal},
		{"polaris.blackboard.fail_task_errors_total", "DebateWorker.FailTask failures", &GlobalBlackboardFailTaskErrorsTotal},
		{"polaris.learning.cursor_errors_total", "Self-improve engine cursor scan failures", &GlobalLearningCursorErrorsTotal},
		{"polaris.learning.skill_register_failures_total", "Synthetic skill registration failures", &GlobalLearningSkillRegisterFailuresTotal},
		{"polaris.gateway.preference_write_failures_total", "Gateway preference/prompt template write failures", &GlobalGatewayPreferenceWriteFailuresTotal},
		{"polaris.store.schema_migration_diag_write_failures_total", "SchemaManager migration_version diagnostic field write failures", &GlobalSchemaMigrationDiagWriteFailuresTotal},
		{"polaris.tool.cron_next_run_write_failures_total", "cron_create next_run_at backfill failures", &GlobalCronNextRunWriteFailuresTotal},
	}
}
