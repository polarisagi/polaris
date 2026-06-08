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
	CircuitBreakerFailureCount    int     `toml:"circuit_breaker.failure_count"`       // 5
	CircuitBreakerCooldownSeconds int     `toml:"circuit_breaker.cooldown_seconds"`    // 10
	CircuitBreakerHalfOpenMax     int     `toml:"circuit_breaker.half_open_max"`       // 1
	PreFlightCostTolerancePct     int     `toml:"pre_flight.cost_tolerance_pct"`       // 15
	TimeoutDialSeconds            int     `toml:"timeout.dial_seconds"`                // 3
	TimeoutTLSSeconds             int     `toml:"timeout.tls_seconds"`                 // 5
	TimeoutResponseHeaderSeconds  int     `toml:"timeout.response_header_seconds"`     // 30
	TimeoutTotalSeconds           int     `toml:"timeout.total_seconds"`               // 120
	MaxStreamBufferKB             int     `toml:"stream.max_buffer_kb"`                // 256
	L1TargetHitRate               float64 `toml:"l1.target_hit_rate"`                  // 0.90
	SemanticCacheMaxEntries       int     `toml:"semantic_cache.max_entries"`          // 10000
	SemanticCacheSimilarity       float64 `toml:"semantic_cache.similarity_threshold"` // 0.95
	SemanticCacheTTLHours         int     `toml:"semantic_cache.ttl_hours"`            // 24
}

type M2StorageThresholds struct {
	SQLiteBusyTimeoutMs   int `toml:"sqlite.busy_timeout_ms"`         // 5000
	SurrealBufferPoolMB   int `toml:"surreal.buffer_pool_mb"`         // 64 — SurrealDB-Core FFI 内存池上限
	EventlogHotDays       int `toml:"eventlog.hot_days"`              // 7
	EventlogWarmDays      int `toml:"eventlog.warm_days"`             // 30
	MaxBatchSize          int `toml:"transaction.max_batch_size"`     // 64
	MaxRowsPerTx          int `toml:"transaction.max_rows_per_tx"`    // 50
	OutboxMaxAttempts     int `toml:"outbox.max_attempts"`            // 5
	MutationBusChannelCap int `toml:"mutation_bus.channel_cap"`       // 4096
	TickerIntervalMs      int `toml:"transaction.ticker_interval_ms"` // 10
	WALCheckpointPages    int `toml:"wal.checkpoint_pages"`           // 1000
}

type M3ObservabilityThresholds struct {
	MemCautionMB  int64 `toml:"memory.caution_mb"`  // 1536 (1.5GB)
	MemWarningMB  int64 `toml:"memory.warning_mb"`  // 1024 (1.0GB)
	MemCriticalMB int64 `toml:"memory.critical_mb"` // 512
	BaselineP95   int64 `toml:"baseline.p95"`       // 200
}

type M4KernelThresholds struct {
	MaxReplanAttempts       int     `toml:"max_replan_attempts"`            // 3
	DefaultBudget           int     `toml:"default_budget"`                 // 50000
	MaxSteps                int     `toml:"max_steps"`                      // 10
	Tier0MaxConcurrent      int     `toml:"tier0_max_concurrent"`           // 4 — 同 max_concurrent_nodes
	SuspendIdleThresholdMin int     `toml:"suspend_idle_threshold_minutes"` // 5
	PlanDAGMaxNodes         int     `toml:"plan_dag.max_nodes"`             // 50
	PlanDAGMaxDepth         int     `toml:"plan_dag.max_depth"`             // 10
	L3WatchdogMaxPerHour    int     `toml:"l3_watchdog.max_per_hour"`       // 10
	WorldModelSkipThreshold float64 `toml:"world_model.skip_threshold"`     // 0.8
	SnapshotIntervalSteps   int     `toml:"snapshot.interval_steps"`        // 1000
	SnapshotRetentionCount  int     `toml:"snapshot.retention_count"`       // 5
}

type M5MemoryThresholds struct {
	EpisodicTTLDays       int `toml:"episodic.ttl_days"`         // 30
	ConsolidationInterval int `toml:"consolidation.interval_ms"` // 60000
	ImmutableCoreMax      int `toml:"core.immutable_max"`        // 100
	RRFK                  int `toml:"rrf.k"`                     // 60 — M5/M10 共享
	GraphMaxDepth         int `toml:"graph.max_depth"`           // 3
}

type M6SkillThresholds struct {
	GoldCacheSize                  int `toml:"cache_size.gold"`          // 5
	SilverCacheSize                int `toml:"cache_size.silver"`        // 20
	BronzeCacheSize                int `toml:"cache_size.bronze"`        // 25
	BronzeCacheTTLMin              int `toml:"cache_ttl.bronze_min"`     // 30
	SkillExecTimeoutLowSeconds     int `toml:"skill_exec.timeout_low_s"` // 30
	SkillExecTimeoutMedHighSeconds int `toml:"skill_exec.timeout_mh_s"`  // 120
}

type M7ToolThresholds struct {
	DefaultSandboxLevel        int  `toml:"sandbox.default_level"` // 2
	DryRunEnabled              bool `toml:"sandbox.dry_run_enabled"`
	MaxWasmMemoryMB            int  `toml:"wasm.max_memory_mb"`            // 256
	MaxWasmWallclockS          int  `toml:"wasm.max_wallclock_s"`          // 60
	DryRunProtectWindowSeconds int  `toml:"dryrun.protect_window_seconds"` // 60
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
}

type M10KnowledgeThresholds struct {
	RAGFinalTopK        int `toml:"rag.final_top_k"`       // 5
	RAGRerankTopM       int `toml:"rag.rerank_top_m"`      // 50
	GraphRAGDailyBudget int `toml:"graphrag.daily_budget"` // 200 — graphrag_llm_call_daily_budget_ht0
	ChunkSize           int `toml:"chunk.size"`            // 256
}

type M11PolicyThresholds struct {
	CapDefaultTTLSeconds        int `toml:"capability.default_ttl_seconds"`    // 300
	AuditRetentionDays          int `toml:"audit.retention_days"`              // 730
	EscalationTimeoutMinutes    int `toml:"escalation.timeout_minutes"`        // 30
	SafeDialerDNSCacheTTLSecond int `toml:"safe_dialer.dns_cache_ttl_seconds"` // 30
	SafeDialerTOCTOUDelayMs     int `toml:"safe_dialer.toctou_delay_ms"`       // 50
	SafeDialerMaxIPsThreshold   int `toml:"safe_dialer.max_ips_threshold"`     // 20
}

// M12EvalThresholds — LLM-as-Judge + 抽样核验阈值。
// SSoT: docs/arch/spec/state.yaml §thresholds.m12_eval
type M12EvalThresholds struct {
	JudgeSingleConfidence float64 `toml:"judge.single_confidence"` // 0.90
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
}

// DefaultThresholds 提供内置默认值（当 config/m*.toml 缺失时使用）。
// 数值与 docs/arch/spec/state.yaml §thresholds 手工同步（ADR-0012 spec_consistency_test 守护核心 SSoT）。
func DefaultThresholds() Thresholds {
	return Thresholds{
		M1Router: M1RouterThresholds{
			CircuitBreakerFailureCount:    5,
			CircuitBreakerCooldownSeconds: 10,
			CircuitBreakerHalfOpenMax:     1,
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
			SQLiteBusyTimeoutMs:   5000,
			SurrealBufferPoolMB:   64,
			EventlogHotDays:       7,
			EventlogWarmDays:      30,
			MaxBatchSize:          64,
			MaxRowsPerTx:          50,
			OutboxMaxAttempts:     5,
			MutationBusChannelCap: 4096,
			TickerIntervalMs:      10,
			WALCheckpointPages:    1000,
		},
		M3Observability: M3ObservabilityThresholds{
			MemCautionMB:  1536,
			MemWarningMB:  1024,
			MemCriticalMB: 512,
			BaselineP95:   200,
		},
		M4Kernel: M4KernelThresholds{
			MaxReplanAttempts:       3,
			DefaultBudget:           50000,
			MaxSteps:                10,
			Tier0MaxConcurrent:      4,
			SuspendIdleThresholdMin: 5,
			PlanDAGMaxNodes:         50,
			PlanDAGMaxDepth:         10,
			L3WatchdogMaxPerHour:    10,
			WorldModelSkipThreshold: 0.8,
			SnapshotIntervalSteps:   1000,
			SnapshotRetentionCount:  5,
		},
		M5Memory: M5MemoryThresholds{
			EpisodicTTLDays:       30,
			ConsolidationInterval: 60000,
			ImmutableCoreMax:      100,
			RRFK:                  60,
			GraphMaxDepth:         3,
		},
		M6Skill: M6SkillThresholds{
			GoldCacheSize:                  5,
			SilverCacheSize:                20,
			BronzeCacheSize:                25,
			BronzeCacheTTLMin:              30,
			SkillExecTimeoutLowSeconds:     30,
			SkillExecTimeoutMedHighSeconds: 120,
		},
		M7Tool: M7ToolThresholds{
			DefaultSandboxLevel:        2,
			DryRunEnabled:              true,
			MaxWasmMemoryMB:            256,
			MaxWasmWallclockS:          60,
			DryRunProtectWindowSeconds: 60,
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
		},
		M10Knowledge: M10KnowledgeThresholds{
			RAGFinalTopK:        5,
			RAGRerankTopM:       50,
			GraphRAGDailyBudget: 200,
			ChunkSize:           256,
		},
		M11Policy: M11PolicyThresholds{
			CapDefaultTTLSeconds:        300,
			AuditRetentionDays:          730,
			EscalationTimeoutMinutes:    30,
			SafeDialerDNSCacheTTLSecond: 30,
			SafeDialerTOCTOUDelayMs:     50,
			SafeDialerMaxIPsThreshold:   20,
		},
		M12Eval: M12EvalThresholds{
			JudgeSingleConfidence: 0.90,
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
		},
	}
}
