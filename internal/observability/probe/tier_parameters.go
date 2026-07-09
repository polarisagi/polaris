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

// Param returns a tier parameter value by name. Used by module code at init time.
// Returns 0 if the parameter name is not recognized.
func (p *TierParameters) Param(name string) int { //nolint:gocyclo
	switch name {
	case "max_concurrent_dag_nodes":
		return p.MaxConcurrentDAGNodes
	case "max_agents":
		return p.MaxAgents
	case "max_replan_attempts":
		return p.MaxReplanAttempts
	case "mem_l0_cache_mb":
		return p.MemL0CacheMB
	case "graph_max_depth":
		return p.GraphMaxDepth
	case "script_worker_max":
		return p.ScriptWorkerMax
	case "max_stream_buffer_kb":
		return p.MaxStreamBufferKB
	case "pipeline_concurrency":
		return p.PipelineConcurrency
	case "graphrag_llm_daily_budget":
		return p.GraphRAGLLMDailyBudget
	case "graphrag_max_entities":
		return p.GraphRAGMaxEntities
	case "regression_budget_min":
		return p.RegressionBudgetMin
	case "pool_intent_handler":
		return p.PoolIntentHandler
	case "pool_ingest":
		return p.PoolIngest
	case "pool_background":
		return p.PoolBackground
	case "pool_eval":
		return p.PoolEval
	case "pool_cron":
		return p.PoolCron
	case "max_blackboard_pending":
		return p.MaxBlackboardPending
	case "max_coordination_token":
		return p.MaxCoordinationToken
	default:
		return 0
	}
}
