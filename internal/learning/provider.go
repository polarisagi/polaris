package learning

import "context"

// 本文件声明 learning 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// learning 包（三环架构：SurpriseIndex + Reflexion + LogicCollapse）
// 需要以下外部能力，通过接口解耦：
//  1. EvalStore    — Eval Harness 存储（自进化案例读写）
//  2. LLMGenerator — 合成评估案例生成（调用 LLM）
//  3. MemoryWriter — 将 Reflexion 结论写入 Episodic 记忆
//  4. SkillWriter  — Logic Collapse 产出写入 Skill 存储
//
// @consumer: learning/engine.go, learning/synthetic/, learning/reflexion/
// @producer: 各具体模块由 cli.go/bootstrap 注入

// EvalCaseSpec learning 包本地定义的 Eval Case 最小规格（防止循环 import eval/harness）。
// eval/harness.EvalCase 在存储层扩展此类型时做字段超集兼容。
type EvalCaseSpec struct {
	ID       string
	TaskType string
	Input    string
	Expected string
	Tags     []string
}

// EvalStore learning 包对 Eval Harness 存储的消费端接口。
// 实现：eval/harness.SQLiteEvalStore（实现时做 EvalCaseSpec → harness.EvalCase 映射）
type EvalStore interface {
	// SaveCase 保存自动生成的 Eval Case（Logic Collapse / SurpriseIndex 触发）。
	SaveCase(ctx context.Context, c *EvalCaseSpec) error
	// LoadPendingCases 加载未运行的 Eval Case（由 Eval Runner 消费）。
	LoadPendingCases(ctx context.Context, limit int) ([]*EvalCaseSpec, error)
}

// LLMGenerator learning 包对合成案例生成的消费端接口。
// 实现：llm.Router（通过 DependencyMap["LLMRouter"] 注入）
type LLMGenerator interface {
	// Generate 通过 LLM 生成合成评估案例（输入样本 + 任务类型 → 变体）。
	Generate(ctx context.Context, seed string, taskType string, count int) ([]string, error)
}

// MemoryWriter learning 包对记忆写入的消费端接口（Reflexion 结论持久化）。
// 实现：memory.MemoryFacade
type MemoryWriter interface {
	// WriteReflexion 将 Reflexion 结论写入 Episodic 记忆（异步 Whisper 通道触发巩固）。
	WriteReflexion(ctx context.Context, taskID, conclusion string, score float64) error
}

// SkillWriter learning 包对 Skill 存储的消费端接口（Logic Collapse 产出写入）。
// 实现：extension/skill.ScriptSkillCache
type SkillWriter interface {
	// StoreScript 将编译后的 Python 脚本写入 Skill 存储（Logic Collapse 产出）。
	StoreScript(name string, script []byte) error
}

// SurpriseMetrics learning 包对 SurpriseIndex 指标上报的消费端接口。
// 实现：observability/metrics.LearningInstrument
type SurpriseMetrics interface {
	// RecordSurpriseIndex 上报当前惊讶指数（供 Prometheus / Grafana 可视化）。
	RecordSurpriseIndex(value float64)
	// RecordReflexionTriggered 记录 Reflexion 被触发的次数。
	RecordReflexionTriggered()
}
