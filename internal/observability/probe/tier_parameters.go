package probe

// TierParameters holds all tier-dependent numeric configuration values.
// 桶 C — 原为各模块文档中"Tier 0=X / Tier 1+=Y"硬编码，现由 AutoConfig 统一选择。
// 对应 spec/state.yaml §thresholds。
type TierParameters struct {
	// M4 Agent Kernel
	MaxConcurrentDAGNodes int `json:"max_concurrent_dag_nodes"`
	MaxAgents             int `json:"max_agents"`
	MaxReplanAttempts     int `json:"max_replan_attempts"`
	IntentChannelBuffer   int `json:"intent_channel_buffer"`
	EventsChannelBuffer   int `json:"events_channel_buffer"`

	// M5 Memory
	MemL0CacheMB        int `json:"mem_l0_cache_mb"`
	GraphMaxDepth       int `json:"graph_max_depth"`
	BackfillConcurrency int `json:"backfill_concurrency"`

	// M6 Skill
	MaxLogicCollapseConcurrent int `json:"max_logic_collapse_concurrent"`
	SkillPreloadGold           int `json:"skill_preload_gold"`
	SkillPreloadSilver         int `json:"skill_preload_silver"`
	SkillPreloadBronze         int `json:"skill_preload_bronze"`

	// M7 Tool
	ScriptWorkerMax   int `json:"script_worker_max"`
	MaxStreamBufferKB int `json:"max_stream_buffer_kb"`

	// M8 Multi-Agent
	MaxBlackboardPending int `json:"max_blackboard_pending"`
	MaxCoordinationToken int `json:"max_coordination_token"`

	// M10 Knowledge RAG
	PipelineConcurrency    int `json:"pipeline_concurrency"`
	GraphRAGLLMDailyBudget int `json:"graphrag_llm_daily_budget"`
	GraphRAGMaxEntities    int `json:"graphrag_max_entities"`

	// M12 Eval
	RegressionBudgetMin int `json:"regression_budget_min"`

	// M13 Scheduler
	PoolIntentHandler int `json:"pool_intent_handler"`
	PoolIngest        int `json:"pool_ingest"`
	PoolBackground    int `json:"pool_background"`
	PoolEval          int `json:"pool_eval"`
	PoolCron          int `json:"pool_cron"`
}

// computeTierParameters selects tier-appropriate numeric defaults.
// 所有参数最终值可被 config.toml 覆盖。
