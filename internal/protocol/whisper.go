package protocol

import "github.com/polarisagi/polaris/pkg/types"

// MemoryWhisperSourceMemoryAgent 标记携带项目与污点元数据的情景记忆线索。
const MemoryWhisperSourceMemoryAgent = "memory_agent"

// MemoryWhisper 来自 MemoryAgent 的耳语线索（异步推送到主脑）。
type MemoryWhisper struct {
	Content       string           // 线索内容（如"检测到类似任务，建议优先查阅 docs/API.md"）
	Source        string           // 线索来源（如"semantic_memory" 或 "episodic_memory"）
	Salience      float64          // 显著度打分（0-1），低于阈值的将被忽略
	SurpriseIndex float64          // OOM 告警等强制介入场景使用高 Surprise Index
	ProjectID     string           // 来源情景事件的有效项目归属
	TaintLevel    types.TaintLevel // 来源污点，接收端只升不降
	MetadataValid bool             // false 时禁止作为 MemoryAgent 情景提示消费
}
