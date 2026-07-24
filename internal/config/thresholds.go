package config

// Thresholds 汇总全模块硬编码阈值的参数化配置。
// 架构文档: ROADMAP.md §3 (P2-06 待对齐工程边界)
// SSoT: docs/arch/spec/state.yaml §thresholds
// 加载优先级: 内置默认值 < config/m*.toml < 环境变量 POLARIS_THRESHOLDS_*
type Thresholds struct {
	M1Router        M1RouterThresholds        `toml:"m1_router"`
	M2Storage       M2StorageThresholds       `toml:"m2_storage"`
	M3Observability M3ObservabilityThresholds `toml:"m3_observability"`
	M4Kernel        M4KernelThresholds        `toml:"m4_kernel"`
	M5Memory        M5MemoryThresholds        `toml:"m5_memory"`
	M6Skill         M6SkillThresholds         `toml:"m6_skill"`
	M7Tool          M7ToolThresholds          `toml:"m7_tool"`
	M8Orchestrator  M8OrchestratorThresholds  `toml:"m8_orchestrator"`
	M9SelfImprove   M9SelfImproveThresholds   `toml:"m9_self_improve"`
	M10Knowledge    M10KnowledgeThresholds    `toml:"m10_knowledge"`
	M11Policy       M11PolicyThresholds       `toml:"m11_policy"`
	M12Eval         M12EvalThresholds         `toml:"m12_eval"`
	M13Interface    M13InterfaceThresholds    `toml:"m13_interface"`
}

type M1RouterThresholds struct {
	CircuitBreakerFailureCount    int     `toml:"circuit_breaker.failure_count"`        // 5
	CircuitBreakerCooldownSeconds int     `toml:"circuit_breaker.cooldown_seconds"`     // 10
	CircuitBreakerHalfOpenMax     int     `toml:"circuit_breaker.half_open_max"`        // 1
	SafecallInferTimeoutSeconds   int     `toml:"safecall_infer_timeout_seconds"`       // 15
	SafecallStreamIdleTimeoutSec  int     `toml:"safecall_stream_idle_timeout_seconds"` // 60
	PreFlightCostTolerancePct     int     `toml:"pre_flight.cost_tolerance_pct"`        // 15
	TimeoutDialSeconds            int     `toml:"timeout.dial_seconds"`                 // 3
	TimeoutTLSSeconds             int     `toml:"timeout.tls_seconds"`                  // 5
	TimeoutResponseHeaderSeconds  int     `toml:"timeout.response_header_seconds"`      // 30
	TimeoutTotalSeconds           int     `toml:"timeout.total_seconds"`                // 120
	MaxStreamBufferKB             int     `toml:"stream.max_buffer_kb"`                 // 256
	L1TargetHitRate               float64 `toml:"l1.target_hit_rate"`                   // 0.90
	SemanticCacheMaxEntries       int     `toml:"semantic_cache.max_entries"`           // 10000
	SemanticCacheSimilarity       float64 `toml:"semantic_cache.similarity_threshold"`  // 0.95
	SemanticCacheTTLHours         int     `toml:"semantic_cache.ttl_hours"`             // 24
}

type M2StorageThresholds struct {
	SQLiteBusyTimeoutMs      int     `toml:"sqlite.busy_timeout_ms"`         // 5000
	SurrealBufferPoolMB      int     `toml:"surreal.buffer_pool_mb"`         // 64 — SurrealDB-Core FFI 内存池上限
	EventlogHotDays          int     `toml:"eventlog.hot_days"`              // 7
	EventlogWarmDays         int     `toml:"eventlog.warm_days"`             // 30
	EventlogDiskWatermarkPct float64 `toml:"eventlog.disk_watermark_pct"`    // 20 — 空闲磁盘占比低于此值才触发归档（Tier-0 磁盘水位门控，P1-3）；<=0 禁用门控
	EventlogHotRowLimit      int64   `toml:"eventlog.hot_row_limit"`         // 1000000 — Hot 表行数超过此值也触发归档（Task 12）
	EventlogHotSizeMB        int64   `toml:"eventlog.hot_size_mb"`           // 500 — Hot 表占用空间超过此值就触发归档（Task 12）
	MaxBatchSize             int     `toml:"transaction.max_batch_size"`     // 64
	MaxRowsPerTx             int     `toml:"transaction.max_rows_per_tx"`    // 50
	OutboxMaxAttempts        int     `toml:"outbox.max_attempts"`            // 5
	OutboxBackoffInitialMs   int64   `toml:"outbox.backoff_initial_ms"`      // 100
	OutboxBackoffMaxMs       int64   `toml:"outbox.backoff_max_ms"`          // 8000
	MutationBusChannelCap    int     `toml:"mutation_bus.channel_cap"`       // 4096
	TickerIntervalMs         int     `toml:"transaction.ticker_interval_ms"` // 10
	WALCheckpointPages       int     `toml:"wal.checkpoint_pages"`           // 1000
}

type M3ObservabilityThresholds struct {
	MemCautionMB  int64 `toml:"memory.caution_mb"`  // 1536 (1.5GB)
	MemWarningMB  int64 `toml:"memory.warning_mb"`  // 1024 (1.0GB)
	MemCriticalMB int64 `toml:"memory.critical_mb"` // 512
	BaselineP95   int64 `toml:"baseline.p95"`       // 200
	TraceExport   struct {
		Enabled  bool   `toml:"enabled"`
		Endpoint string `toml:"endpoint"`
	} `toml:"trace_export"`
}

type M4KernelThresholds struct {
	MaxReplanAttempts             int     `toml:"max_replan_attempts"`            // 3
	DefaultBudget                 int     `toml:"default_budget"`                 // 50000
	MaxSteps                      int     `toml:"max_steps"`                      // 10
	Tier0MaxConcurrent            int     `toml:"tier0_max_concurrent"`           // 4 — 同 max_concurrent_nodes
	SuspendIdleThresholdMin       int     `toml:"suspend_idle_threshold_minutes"` // 5
	PlanDAGMaxNodes               int     `toml:"plan_dag.max_nodes"`             // 50
	PlanDAGMaxDepth               int     `toml:"plan_dag.max_depth"`             // 10
	L3WatchdogMaxPerHour          int     `toml:"l3_watchdog.max_per_hour"`       // 10
	WorldModelSkipThreshold       float64 `toml:"world_model.skip_threshold"`     // 0.8
	SnapshotIntervalSteps         int     `toml:"snapshot.interval_steps"`        // 1000
	SnapshotRetentionCount        int     `toml:"snapshot.retention_count"`       // 5
	ReplanExtensionActivationSecs int     `toml:"replan_extension_activation_s"`  // 3
	SurpriseHintThreshold         float64 `toml:"surprise_hint_threshold"`        // 0.6

	// PRM（ProcessRewardModel）S_PLAN 候选 DAG 选优，见 docs/arch/M04-Agent-Kernel.md §4.6。
	// 默认关闭须显式开启（文档原文）：并发多候选打分会成倍增加 token 消耗，Operator 需明确知情后开启。
	PRMEnabled        bool    `toml:"prm.enabled"`         // false
	PRMComplexityGate float64 `toml:"prm.complexity_gate"` // 0.5 — 任务复杂度低于此值直接跳过，零额外开销
	PRMMaxCandidates  int     `toml:"prm.max_candidates"`  // 3 — 文档: "研究数据显示 3 候选 ROI 最优"
	PRMMinThreshold   float64 `toml:"prm.min_threshold"`   // 0.4 — 全部候选低于此分数时兜底取第一个候选
	PRMScorerModel    string  `toml:"prm.scorer_model"`    // "" — 留空则沿用 Provider 默认路由，不强制指定 budget-tier 模型名
}

type M5MemoryThresholds struct {
	EpisodicTTLDays       int `toml:"episodic.ttl_days"`         // 30
	ConsolidationInterval int `toml:"consolidation.interval_ms"` // 60000
	ImmutableCoreMax      int `toml:"core.immutable_max"`        // 100
	CoreMemoryBlockMaxKB  int `toml:"core.memory_block_max_kb"`  // 2
	CoreMemoryTotalMaxKB  int `toml:"core.memory_total_max_kb"`  // 8
	RRFK                  int `toml:"rrf.k"`                     // 60 — M5/M10 共享
	GraphMaxDepth         int `toml:"graph.max_depth"`           // 3
	// ReflectionTaskTypeWhitelist/ReflectionMinReplanCount — §3.4 ReflectionWorker
	// 触发策略（2026-07-21 deadcode 审查补齐，此前 reflexion.NewReflectionWorkerWithConfig
	// 有完整实现+测试但生产侧无配置来源，一直只能走硬编码默认值）。
	ReflectionTaskTypeWhitelist []string `toml:"reflection.task_type_whitelist"` // ["complex_reasoning","coding","research","debug","analysis"]
	ReflectionMinReplanCount    int      `toml:"reflection.min_replan_count"`    // 2
	// DriftCheckIntervalHours/DriftThreshold/DriftAnchorSampleRate — §12.3
	// DriftDetector 漂移响应编排器配置（2026-07-21 deadcode 审查补齐，此前
	// DriftDetector/EmbeddingVersionTracker 全套实现完整但零生产调用点，缺整条
	// "周期性喂 anchor → Detect() → 按 task_type 降级 BM25 → Blue-Green 重嵌"编排链）。
	DriftCheckIntervalHours int     `toml:"drift.check_interval_hours"` // 168 (7d)
	DriftThreshold          float64 `toml:"drift.threshold"`            // 0.05
	// DriftAnchorSampleRate 每次 HybridRetriever.Search 命中后采样为新 anchor 的概率
	// （自参照基线：Expected 取采样当下的 Top-5 结果，而非外部标注的绝对真值——
	// 漂移定义为"当前检索结果相对历史自身基线的显著偏离"）。
	DriftAnchorSampleRate float64 `toml:"drift.anchor_sample_rate"` // 0.02
}

type M6SkillThresholds struct {
	GoldCacheSize                  int     `toml:"cache_size.gold"`                        // 5
	SilverCacheSize                int     `toml:"cache_size.silver"`                      // 20
	BronzeCacheSize                int     `toml:"cache_size.bronze"`                      // 25
	BronzeCacheTTLMin              int     `toml:"cache_ttl.bronze_min"`                   // 30
	SkillExecTimeoutLowSeconds     int     `toml:"skill_exec.timeout_low_s"`               // 30
	SkillExecTimeoutMedHighSeconds int     `toml:"skill_exec.timeout_mh_s"`                // 120
	EvolutionSuccessThreshold      float64 `toml:"skill_exec.evolution_success_threshold"` // 0.6
	EvolutionMinUsage              int     `toml:"skill_exec.evolution_min_usage"`         // 10
}

type M7ToolThresholds struct {
	DefaultSandboxLevel        int  `toml:"sandbox.default_level"` // 3 (L3 Container；macOS 降级至 L2 Wasmtime)
	DryRunEnabled              bool `toml:"sandbox.dry_run_enabled"`
	MaxScriptMemoryMB          int  `toml:"script.max_memory_mb"`          // 256
	MaxScriptWallclockS        int  `toml:"script.max_wallclock_s"`        // 60
	DryRunProtectWindowSeconds int  `toml:"dryrun.protect_window_seconds"` // 60
	MaxCodeSizeBytes           int  `toml:"max_code_size_bytes"`           // 16384
}

type M8OrchestratorThresholds struct {
	LeaseTTLSeconds             int `toml:"lease.ttl_seconds"`                 // 60
	HeartbeatSeconds            int `toml:"heartbeat.seconds"`                 // 15
	HeartbeatJitter             int `toml:"heartbeat.jitter"`                  // 5
	ReaperScanInterval          int `toml:"reaper.scan_interval_ms"`           // 1000
	MaxAgentsDesktop            int `toml:"agents.max_desktop"`                // 2
	MaxAgentsServer             int `toml:"agents.max_server"`                 // 3
	AgentRestartMaxInWindow     int `toml:"supervisor.restart_max_in_window"`  // 3
	AgentRestartWindowSeconds   int `toml:"supervisor.restart_window_seconds"` // 60
	SupervisorBackoffInitialMs  int `toml:"supervisor.backoff_initial_ms"`     // 200
	SupervisorBackoffMaxSeconds int `toml:"supervisor.backoff_max_seconds"`    // 60
	CompensationTimeoutSeconds  int `toml:"compensation.timeout_seconds"`      // 300
	CompensationPollSeconds     int `toml:"compensation.poll_seconds"`         // 5
}

// M9SelfImproveThresholds — 后台自演化 worker 调度 + Canary rollout 参数。
// SSoT: docs/arch/spec/state.yaml §thresholds.m9_self_improve
type M9SelfImproveThresholds struct {
	WorkerCPUPctUserActive         float64   `toml:"worker.cpu_pct_user_active"`         // 0.05
	WorkerCPUPctIdle               float64   `toml:"worker.cpu_pct_idle"`                // 0.50
	WorkerHeartbeatSeconds         int       `toml:"worker.heartbeat_seconds"`           // 30
	WorkerRestartBackoffInitialMs  int       `toml:"worker.restart_backoff_initial_ms"`  // 200
	WorkerRestartBackoffMaxSeconds int       `toml:"worker.restart_backoff_max_seconds"` // 60
	CanarySteps                    []float64 `toml:"canary.steps"`                       // [0.01, 0.10, 0.50, 1.00]
	CanaryDwellPerStepHoursHT0     int       `toml:"canary.dwell_per_step_hours_ht0"`    // 1
	// SurpriseIndex 双通道路由阈值（System 1 / 混合 / System 2）
	SurpriseRouteLowThreshold  float64 `toml:"surprise_route.low_threshold"`  // 0.30
	SurpriseRouteHighThreshold float64 `toml:"surprise_route.high_threshold"` // 0.60

	// QLoRATrainBatchSize/PRMTrainBatchSize — 条件梯度训练批次触发阈值
	// （M09-Self-Improvement-Engine.md §4；2026-07-21 deadcode 审查补齐上游
	// 样本采集+触发流水线）。累积样本数达到该值时异步触发一次
	// QLoRAAdapter.Train/PRMAdapter.Train，随后清空缓冲区重新累积。文档本身
	// 未给出具体数值（§4 只定义了 Tier 门控与请求体形状），此处为提议的默认值：
	// 64 是 LoRA/PRM 小批次微调的常见量级下限，同时不会让样本在真实使用频率下
	// 长期不触发（QLoRA 样本来自 replaySuccess 的"经 replan 后成功"轨迹，属低频
	// 事件；PRM 样本来自 M12 §9 的 1% 生产流量抽样评分，属更低频事件——两者
	// 都可能需要相当长时间才累积到批次量，这是"后台慢速自演化"特性本身的一部分，
	// 非本实现的缺陷）。
	QLoRATrainBatchSize int `toml:"qlora.train_batch_size"` // 64
	PRMTrainBatchSize   int `toml:"prm.train_batch_size"`   // 64
}

type M10KnowledgeThresholds struct {
	RAGFinalTopK        int `toml:"rag.final_top_k"`       // 5
	RAGRerankTopM       int `toml:"rag.rerank_top_m"`      // 50
	GraphRAGDailyBudget int `toml:"graphrag.daily_budget"` // 200 — graphrag_llm_call_daily_budget_ht0
	ChunkSize           int `toml:"chunk.size"`            // 256
}

type M11PolicyThresholds struct {
	CapDefaultTTLSeconds         int `toml:"capability.default_ttl_seconds"`    // 300
	AuditRetentionDays           int `toml:"audit.retention_days"`              // 730
	EscalationTimeoutMinutes     int `toml:"escalation.timeout_minutes"`        // 30
	L3ImprovementCooldownSeconds int `toml:"l3_improvement_cooldown_seconds"`   // 600 - Task 21: mandatory cooldown for L3 hitl
	SafeDialerDNSCacheTTLSecond  int `toml:"safe_dialer.dns_cache_ttl_seconds"` // 30
	SafeDialerTOCTOUDelayMs      int `toml:"safe_dialer.toctou_delay_ms"`       // 50
	SafeDialerMaxIPsThreshold    int `toml:"safe_dialer.max_ips_threshold"`     // 20
}

// M12EvalThresholds — LLM-as-Judge + 抽样核验阈值。
// SSoT: docs/arch/spec/state.yaml §thresholds.m12_eval
type M12EvalThresholds struct {
	JudgeSingleConfidence   float64 `toml:"judge.single_confidence"`    // 0.90
	ShadowSampleRate        float64 `toml:"shadow.sample_rate"`         // 0.01
	ShadowPassRateThreshold float64 `toml:"shadow.pass_rate_threshold"` // 0.95
	ShadowMinSamples        int     `toml:"shadow.min_samples"`         // 10
	EvalLLMJudgeTimeoutSec  int     `toml:"eval_llm_judge_timeout_sec"` // 15
	ShadowInferTimeoutSec   int     `toml:"shadow_infer_timeout_sec"`   // 30

	// V8-S2 Meta-Eval Sentinel（meta_holdout 审计，见 internal/eval/analysis/meta_eval.go）。
	// MetaAuditGateEnabled 默认 false：这是新功能，需要运维先生成 meta_auditor 密钥对
	// 并至少成功运行过一次 `polaris eval meta-audit run` 才有意义；默认开启会让所有
	// 既有部署（从未跑过 meta_audit）永久卡在 Gate2 无法推进 Canary，属于不必要的破坏性变更。
	MetaAuditGateEnabled bool `toml:"meta_audit.gate_enabled"`  // false
	MetaAuditMaxAgeHours int  `toml:"meta_audit.max_age_hours"` // 168 (7天)——超过此新鲜度 AdvanceGate 视为 stale，fail-closed 停在当前 Gate
}

type M13InterfaceThresholds struct {
	ReadTimeoutSeconds             int `toml:"timeout.read_seconds"`              // 10
	WriteTimeoutSeconds            int `toml:"timeout.write_seconds"`             // 60
	IdleTimeoutSeconds             int `toml:"timeout.idle_seconds"`              // 120
	GracefulShutdownTimeoutSeconds int `toml:"timeout.graceful_shutdown_seconds"` // 30
	HITLDefaultDeadlineMinUrgent   int `toml:"hitl.default_deadline_min_urgent"`  // 5
	HITLDefaultDeadlineMinNormal   int `toml:"hitl.default_deadline_min_normal"`  // 60
	HITLDefaultDeadlineMinLong     int `toml:"hitl.default_deadline_min_long"`    // 1440
	WorkerIntentHandler            int `toml:"worker.intent_handler"`             // 4
	WorkerIngest                   int `toml:"worker.ingest"`                     // 2
	WorkerBackground               int `toml:"worker.background"`                 // 2
	WorkerEval                     int `toml:"worker.eval"`                       // 1
	WorkerCron                     int `toml:"worker.cron"`                       // 1
	MaxConcurrentLLMCalls          int `toml:"max_concurrent_llm_calls"`          // 4
	LazyLoadToolThreshold          int `toml:"lazy_load_tool_threshold"`          // 40
}

// DefaultThresholds 提供内置默认值（当 config/m*.toml 缺失时使用）。
// 数值与 docs/arch/spec/state.yaml §thresholds 手工同步（ADR-0012 spec_consistency_test 守护核心 SSoT）。
func DefaultThresholds() Thresholds {
	return Thresholds{
		M1Router: M1RouterThresholds{
			CircuitBreakerFailureCount:    5,
			CircuitBreakerCooldownSeconds: 10,
			CircuitBreakerHalfOpenMax:     1,
			SafecallInferTimeoutSeconds:   15,
			SafecallStreamIdleTimeoutSec:  60,
			PreFlightCostTolerancePct:     15,
			TimeoutDialSeconds:            3,
			TimeoutTLSSeconds:             5,
			TimeoutResponseHeaderSeconds:  30,
			TimeoutTotalSeconds:           120,
			MaxStreamBufferKB:             256,
			L1TargetHitRate:               0.90,
			SemanticCacheMaxEntries:       10000,
			SemanticCacheSimilarity:       0.95,
			SemanticCacheTTLHours:         24,
		},
		M2Storage: M2StorageThresholds{
			SQLiteBusyTimeoutMs:      5000,
			SurrealBufferPoolMB:      64,
			EventlogHotDays:          7,
			EventlogWarmDays:         30,
			EventlogDiskWatermarkPct: 20,
			EventlogHotRowLimit:      1000000,
			EventlogHotSizeMB:        500,
			MaxBatchSize:             64,
			MaxRowsPerTx:             50,
			OutboxMaxAttempts:        5,
			OutboxBackoffInitialMs:   100,
			OutboxBackoffMaxMs:       8000,
			MutationBusChannelCap:    4096,
			TickerIntervalMs:         10,
			WALCheckpointPages:       1000,
		},
		M3Observability: M3ObservabilityThresholds{
			MemCautionMB:  1536,
			MemWarningMB:  1024,
			MemCriticalMB: 512,
			BaselineP95:   200,
			TraceExport: struct {
				Enabled  bool   `toml:"enabled"`
				Endpoint string `toml:"endpoint"`
			}{
				Enabled:  false,
				Endpoint: "",
			},
		},
		M4Kernel: M4KernelThresholds{
			MaxReplanAttempts:             3,
			DefaultBudget:                 50000,
			MaxSteps:                      10,
			Tier0MaxConcurrent:            4,
			SuspendIdleThresholdMin:       5,
			PlanDAGMaxNodes:               50,
			PlanDAGMaxDepth:               10,
			L3WatchdogMaxPerHour:          10,
			WorldModelSkipThreshold:       0.8,
			SnapshotIntervalSteps:         1000,
			SnapshotRetentionCount:        5,
			ReplanExtensionActivationSecs: 3,
			SurpriseHintThreshold:         0.6,
			PRMEnabled:                    false,
			PRMComplexityGate:             0.5,
			PRMMaxCandidates:              3,
			PRMMinThreshold:               0.4,
			PRMScorerModel:                "",
		},
		M5Memory: M5MemoryThresholds{
			EpisodicTTLDays:       30,
			ConsolidationInterval: 60000,
			ImmutableCoreMax:      100,
			CoreMemoryBlockMaxKB:  2,
			CoreMemoryTotalMaxKB:  8,
			RRFK:                  60,
			GraphMaxDepth:         3,
			ReflectionTaskTypeWhitelist: []string{
				"complex_reasoning", "coding", "research", "debug", "analysis",
			},
			ReflectionMinReplanCount: 2,
			DriftCheckIntervalHours:  168,
			DriftThreshold:           0.05,
			DriftAnchorSampleRate:    0.02,
		},
		M6Skill: M6SkillThresholds{
			GoldCacheSize:                  5,
			SilverCacheSize:                20,
			BronzeCacheSize:                25,
			BronzeCacheTTLMin:              30,
			SkillExecTimeoutLowSeconds:     30,
			SkillExecTimeoutMedHighSeconds: 120,
			EvolutionSuccessThreshold:      0.6,
			EvolutionMinUsage:              10,
		},
		M7Tool: M7ToolThresholds{
			DefaultSandboxLevel:        3,
			DryRunEnabled:              true,
			MaxScriptMemoryMB:          256,
			MaxScriptWallclockS:        60,
			DryRunProtectWindowSeconds: 60,
			MaxCodeSizeBytes:           16384,
		},
		M8Orchestrator: M8OrchestratorThresholds{
			LeaseTTLSeconds:             60,
			HeartbeatSeconds:            15,
			HeartbeatJitter:             5,
			ReaperScanInterval:          1000,
			MaxAgentsDesktop:            2,
			MaxAgentsServer:             3,
			AgentRestartMaxInWindow:     3,
			AgentRestartWindowSeconds:   60,
			SupervisorBackoffInitialMs:  200,
			SupervisorBackoffMaxSeconds: 60,
			CompensationTimeoutSeconds:  300,
			CompensationPollSeconds:     5,
		},
		M9SelfImprove: M9SelfImproveThresholds{
			WorkerCPUPctUserActive:         0.05,
			WorkerCPUPctIdle:               0.50,
			WorkerHeartbeatSeconds:         30,
			WorkerRestartBackoffInitialMs:  200,
			WorkerRestartBackoffMaxSeconds: 60,
			CanarySteps:                    []float64{0.01, 0.10, 0.50, 1.00},
			CanaryDwellPerStepHoursHT0:     1,
			SurpriseRouteLowThreshold:      0.30,
			SurpriseRouteHighThreshold:     0.60,
			QLoRATrainBatchSize:            64,
			PRMTrainBatchSize:              64,
		},
		M10Knowledge: M10KnowledgeThresholds{
			RAGFinalTopK:        5,
			RAGRerankTopM:       50,
			GraphRAGDailyBudget: 200,
			ChunkSize:           256,
		},
		M11Policy: M11PolicyThresholds{
			CapDefaultTTLSeconds:         300,
			AuditRetentionDays:           730,
			EscalationTimeoutMinutes:     30,
			L3ImprovementCooldownSeconds: 600,
			SafeDialerDNSCacheTTLSecond:  30,
			SafeDialerTOCTOUDelayMs:      50,
			SafeDialerMaxIPsThreshold:    20,
		},
		M12Eval: M12EvalThresholds{
			JudgeSingleConfidence:   0.90,
			ShadowSampleRate:        0.01,
			ShadowPassRateThreshold: 0.95,
			ShadowMinSamples:        10,
			EvalLLMJudgeTimeoutSec:  15,
			ShadowInferTimeoutSec:   30,
			MetaAuditGateEnabled:    false,
			MetaAuditMaxAgeHours:    168,
		},
		M13Interface: M13InterfaceThresholds{
			ReadTimeoutSeconds:             10,
			WriteTimeoutSeconds:            60,
			IdleTimeoutSeconds:             120,
			GracefulShutdownTimeoutSeconds: 30,
			HITLDefaultDeadlineMinUrgent:   5,
			HITLDefaultDeadlineMinNormal:   60,
			HITLDefaultDeadlineMinLong:     1440,
			WorkerIntentHandler:            4,
			WorkerIngest:                   2,
			WorkerBackground:               2,
			WorkerEval:                     1,
			WorkerCron:                     1,
			MaxConcurrentLLMCalls:          4,
			LazyLoadToolThreshold:          40,
		},
	}
}
