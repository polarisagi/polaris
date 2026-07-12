package swarm

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 swarm 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// swarm 包需要以下外部能力：
//   1. OutboxWriter — 向 SQLite outbox 发布事件（跨模块异步通信）
//   2. LLMInfer     — Orchestrator 做决策时调用 LLM 推理（规划/分配）
//   3. EvalMetrics  — Swarm 层指标上报
//
// @consumer: execute/orchestrator（外部模块）、swarm/planner、swarm/supervisor
// @producer: 各具体模块由 cli.go/bootstrap 注入

// OutboxWriter swarm 包对异步事件队列的消费端接口。
// 实现：store/repo.SQLiteOutboxRepo
// 禁止：swarm 直接 import store/repo（防止 L2→L0 直接依赖具体实现）
type OutboxWriter interface {
	// Publish 向 outbox 表插入一条事件（保证最终投递，WAL 安全）。
	Publish(ctx context.Context, event *types.OutboxEvent) error
}

// LLMInfer swarm 包对 LLM 推理能力的消费端接口（规划/任务分配决策）。
// 实现：llm.Router（通过 DependencyMap["LLMRouter"] 注入）
// 禁止：swarm 直接 import llm/adapter 具体包
type LLMInfer interface {
	// Infer 发送推理请求，返回 LLM 响应文本。
	Infer(ctx context.Context, req *types.InferRequest) (*types.InferResponse, error)
}

// SwarmMetrics swarm 包对 Prometheus 指标上报的消费端接口。
// 实现：observability/metrics.SwarmInstrument
// nil 时静默（单元测试/最小化部署）。
type SwarmMetrics interface {
	// RecordTaskPosted 记录任务投递到 Blackboard（按优先级分桶）。
	RecordTaskPosted(priority int)
	// RecordTaskCompleted 记录任务完成（含耗时）。
	RecordTaskCompleted(agentID string, durationMs int64)
	// RecordWorkerCount 更新当前活跃 Worker 数量（Gauge）。
	RecordWorkerCount(count int)
}
