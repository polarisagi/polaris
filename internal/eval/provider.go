package eval

import (
	"context"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 eval 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// eval 包（评估 + 回归检测）需要以下外部能力：
//   1. AgentRunner  — 运行 Agent 执行 EvalCase（实际推理路径）
//   2. EvalStorage  — EvalCase/RunMetrics 持久化
//   3. SamplingHook — 影子采样（生产流量 1% 重放）
//
// @consumer: eval/harness/runner.go, eval/analysis/
// @producer: 各具体模块由 cli.go/bootstrap 注入

// AgentRunner eval 包对 Agent 执行的消费端接口。
// 实现：agent.Agent（通过 DependencyMap["AgentFacade"] 注入）
// 禁止：eval 直接 import agent（防止 eval→agent 循环）
type AgentRunner interface {
	// RunEval 以 EvalCase.Input 为输入运行 Agent，返回输出文本和工具调用序列。
	RunEval(ctx context.Context, input []byte) (output []byte, toolSeq []string, err error)
}

// EvalStorage eval 包对持久化存储的消费端接口。
// 实现：eval/harness.SQLiteEvalStore
type EvalStorage interface {
	// SaveCase 保存 EvalCase（由 learning/synthetic 写入）。
	SaveCase(ctx context.Context, c *harness.EvalCase) error
	// LoadPending 加载未执行的 EvalCase（limit 控制每批大小）。
	LoadPending(ctx context.Context, limit int) ([]*harness.EvalCase, error)
	// SaveMetrics 保存一次运行的汇总指标。
	SaveMetrics(ctx context.Context, runID string, m *harness.RunMetrics) error
}

// SamplingHook eval 包对影子采样的消费端接口（生产流量重放）。
// 实现：eval/analysis.SamplingMonitor（1% 采样率，不影响生产延迟）
type SamplingHook interface {
	// Record 记录一次生产请求（异步，不阻塞）。
	Record(ctx context.Context, input []byte, output *types.ProviderResponse)
	// SamplingRate 返回当前采样率（0.0-1.0）。
	SamplingRate() float64
}
