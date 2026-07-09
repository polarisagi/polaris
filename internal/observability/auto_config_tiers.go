package observability

import "github.com/polarisagi/polaris/internal/observability/probe"

// ============================================================================
// Tier 数值参数表（R7 拆分自 auto_config.go）：按硬件分级设置的静态参数矩阵。
// 结构体/构造/静态配置计算见 auto_config.go；内存压力回调见 auto_config_pressure.go。
// ============================================================================

func (ac *AutoConfig) computeTierParameters(p *probe.TierParameters) {
	switch ac.Probe.Tier {
	case probe.Tier3: // 64GB+
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
		p.ScriptWorkerMax = 16
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

	case probe.Tier2: // 24GB+
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
		p.ScriptWorkerMax = 12
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

	case probe.Tier1: // 16GB
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
		p.ScriptWorkerMax = 8
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

	default: // probe.Tier0 8GB
		p.MaxConcurrentDAGNodes = 4
		p.MaxAgents = 3
		p.MaxReplanAttempts = 3
		p.IntentChannelBuffer = 8
		p.EventsChannelBuffer = 32
		p.MemL0CacheMB = 80
		p.GraphMaxDepth = 3
		p.BackfillConcurrency = 1
		p.MaxLogicCollapseConcurrent = 1 // LogicCollapse 在 probe.Tier0 启用，单并发限制编译期内存峰值
		p.SkillPreloadGold = 5
		p.SkillPreloadSilver = 20
		p.SkillPreloadBronze = 25
		p.ScriptWorkerMax = 4
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
