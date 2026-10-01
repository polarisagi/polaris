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
		p.GraphRAGConcurrentWorkers = 8
		p.GraphRAGMaxEntities = 500000
		p.RegressionBudgetMin = 30
		p.PoolIntentHandler = 15
		p.PoolIngest = 12
		p.PoolBackground = 20
		p.PoolEval = 6
		p.PoolCron = 6
		p.STTNumThreads = 4
		p.TTSPrefetchCount = 3

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
		p.GraphRAGConcurrentWorkers = 4
		p.GraphRAGMaxEntities = 200000
		p.RegressionBudgetMin = 30
		p.PoolIntentHandler = 10
		p.PoolIngest = 8
		p.PoolBackground = 15
		p.PoolEval = 4
		p.PoolCron = 4
		p.STTNumThreads = 4
		p.TTSPrefetchCount = 3

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
		p.GraphRAGConcurrentWorkers = 2
		p.GraphRAGMaxEntities = 50000
		p.RegressionBudgetMin = 20
		p.PoolIntentHandler = 5
		p.PoolIngest = 5
		p.PoolBackground = 10
		p.PoolEval = 2
		p.PoolCron = 2
		p.STTNumThreads = 2
		p.TTSPrefetchCount = 2

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
		p.GraphRAGConcurrentWorkers = 1
		p.GraphRAGMaxEntities = 50000
		p.RegressionBudgetMin = 10
		p.PoolIntentHandler = 5
		p.PoolIngest = 5
		p.PoolBackground = 10
		p.PoolEval = 2
		p.PoolCron = 2
		p.STTNumThreads = 1
		p.TTSPrefetchCount = 1
	}

	// TTS 线程数不随内存档位走，只取决于逻辑核数：min(4, 核数)（audio-v2-spec §2.3）。
	// Kokoro fp32 实测 RTF 0.42@4 线程 / 0.59@2 线程，核数充足时 4 线程是性价比拐点；
	// 内存档位高并不代表核多（16GB 的 2 核云主机开 4 线程只会互相抢核）。
	p.TTSNumThreads = min(4, max(1, ac.Probe.CPUCores))
}
