package observability

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
	WasmPoolMax       int `json:"wasm_pool_max"`
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
func (ac *AutoConfig) computeTierParameters(p *TierParameters) {
	switch ac.Probe.Tier {
	case Tier3: // 64GB+
		p.MaxConcurrentDAGNodes = 16
		p.MaxAgents = 12
		p.MaxReplanAttempts = 4
		p.IntentChannelBuffer = 32
		p.EventsChannelBuffer = 128
		p.MemL0CacheMB = 512
		p.GraphMaxDepth = 6
		p.BackfillConcurrency = 4
		p.MaxLogicCollapseConcurrent = 4
		p.SkillPreloadGold = 20
		p.SkillPreloadSilver = 80
		p.SkillPreloadBronze = 200
		p.WasmPoolMax = 16
		p.MaxStreamBufferKB = 1024
		p.MaxBlackboardPending = 1024
		p.MaxCoordinationToken = 500000
		p.PipelineConcurrency = 8
		p.GraphRAGLLMDailyBudget = 1000
		p.GraphRAGMaxEntities = 500000
		p.RegressionBudgetMin = 30
		p.PoolIntentHandler = 15
		p.PoolIngest = 12
		p.PoolBackground = 20
		p.PoolEval = 6
		p.PoolCron = 6

	case Tier2: // 24GB+
		p.MaxConcurrentDAGNodes = 12
		p.MaxAgents = 8
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 24
		p.EventsChannelBuffer = 96
		p.MemL0CacheMB = 256
		p.GraphMaxDepth = 5
		p.BackfillConcurrency = 3
		p.MaxLogicCollapseConcurrent = 4
		p.SkillPreloadGold = 15
		p.SkillPreloadSilver = 60
		p.SkillPreloadBronze = 150
		p.WasmPoolMax = 12
		p.MaxStreamBufferKB = 1024
		p.MaxBlackboardPending = 512
		p.MaxCoordinationToken = 350000
		p.PipelineConcurrency = 6
		p.GraphRAGLLMDailyBudget = 500
		p.GraphRAGMaxEntities = 200000
		p.RegressionBudgetMin = 30
		p.PoolIntentHandler = 10
		p.PoolIngest = 8
		p.PoolBackground = 15
		p.PoolEval = 4
		p.PoolCron = 4

	case Tier1: // 16GB
		p.MaxConcurrentDAGNodes = 8
		p.MaxAgents = 5
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 16
		p.EventsChannelBuffer = 64
		p.MemL0CacheMB = 160
		p.GraphMaxDepth = 4
		p.BackfillConcurrency = 2
		p.MaxLogicCollapseConcurrent = 2
		p.SkillPreloadGold = 10
		p.SkillPreloadSilver = 40
		p.SkillPreloadBronze = 100
		p.WasmPoolMax = 8
		p.MaxStreamBufferKB = 512
		p.MaxBlackboardPending = 256
		p.MaxCoordinationToken = 200000
		p.PipelineConcurrency = 4
		p.GraphRAGLLMDailyBudget = 200
		p.GraphRAGMaxEntities = 50000
		p.RegressionBudgetMin = 20
		p.PoolIntentHandler = 5
		p.PoolIngest = 5
		p.PoolBackground = 10
		p.PoolEval = 2
		p.PoolCron = 2

	default: // Tier0 8GB
		p.MaxConcurrentDAGNodes = 4
		p.MaxAgents = 3
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 8
		p.EventsChannelBuffer = 32
		p.MemL0CacheMB = 80
		p.GraphMaxDepth = 3
		p.BackfillConcurrency = 1
		p.MaxLogicCollapseConcurrent = 0 // Logic Collapse disabled at Tier0
		p.SkillPreloadGold = 5
		p.SkillPreloadSilver = 20
		p.SkillPreloadBronze = 25
		p.WasmPoolMax = 4
		p.MaxStreamBufferKB = 256
		p.MaxBlackboardPending = 128
		p.MaxCoordinationToken = 100000
		p.PipelineConcurrency = 2
		p.GraphRAGLLMDailyBudget = 200
		p.GraphRAGMaxEntities = 50000
		p.RegressionBudgetMin = 10
		p.PoolIntentHandler = 5
		p.PoolIngest = 5
		p.PoolBackground = 10
		p.PoolEval = 2
		p.PoolCron = 2
	}
}

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
	case "wasm_pool_max":
		return p.WasmPoolMax
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
