package eval

import (
	"context"

	"github.com/polarisagi/polaris/internal/eval/harness"
)

// EvalFacade Eval Harness 模块对外统一接口。
//
// 问题背景：
//
//	当前 eval 包的入口分散（harness.Runner / harness.SQLiteEvalStore / analysis.SamplingMonitor），
//	上层代码（gateway/server、learning/engine）直接持有具体 struct。
//
// 解决方案：
//   - EvalFacade 是 eval 包对外的统一入口
//   - 上层模块依赖此接口，不直接持有 harness.Runner/SQLiteEvalStore
//   - 内部评估流程（采样 → 运行 → 回归检测）对外透明
//
// @consumer: gateway/server/server.go, learning/engine.go, automation/scheduler.go
// @producer: eval.DefaultEvalRunner（由 cli.go/bootstrap 构造注入）
type EvalFacade interface {
	// RunCase 立即执行单个 EvalCase（同步，用于冒烟测试/HITL 触发）。
	RunCase(ctx context.Context, c *harness.EvalCase) (*harness.RunMetrics, error)

	// RunPending 批量执行所有 pending 的 EvalCase（Eval Harness 定期调度入口）。
	// 返回本次运行的汇总指标。
	RunPending(ctx context.Context, limit int) (*harness.RunMetrics, error)

	// SaveCase 保存一个 EvalCase（由 learning/synthetic 生成后写入）。
	SaveCase(ctx context.Context, c *harness.EvalCase) error

	// CheckRegression 对比 baseline 和当前指标，返回回归报告（nil = 无回归）。
	CheckRegression(ctx context.Context, baseline, current *harness.RunMetrics) *harness.RegressionAlert

	// ActiveRunner 返回底层 Runner（供需要细粒度操作的代码使用）。
	ActiveRunner() harness.Runner
}
