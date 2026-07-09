package learning

import "context"

// LearningFacade 自我学习模块对外统一接口。
//
// 问题背景：
//
//	当前 learning.Engine 对外暴露了大量 SetXxx 方法（15+），调用方必须了解内部实现细节
//	才能正确初始化引擎。上层代码（agent、gateway）直接持有 *Engine struct。
//
// 解决方案：
//   - LearningFacade 是 learning 包对外的统一入口接口
//   - 上层模块依赖此接口，不直接持有 *Engine
//   - 内部三环架构（SurpriseIndex + Reflexion + LogicCollapse）对外透明
//
// @consumer: agent/agent.go, gateway/server/server.go, automation/
// @producer: learning.Engine（由 cli.go/bootstrap 构造注入）
type LearningFacade interface {
	// Start 启动自进化引擎（三环异步 Worker）。
	// ctx 取消时引擎优雅退出。
	Start(ctx context.Context) error

	// ReportOutcome 上报一次任务执行结果（触发 SurpriseIndex 计算 + 可能的 Reflexion）。
	ReportOutcome(ctx context.Context, taskID string, result *TaskResult) error

	// SurpriseIndex 返回当前系统惊讶指数（0=稳定, 1=高度不稳定）。
	// 供 FSM / Gateway 动态调节请求速率。
	SurpriseIndex() float64

	// TriggerCurriculum 手动触发课程生成（自动化测试/Eval Harness 驱动）。
	TriggerCurriculum(ctx context.Context) error

	// Stop 优雅停止引擎（等待后台 Worker 退出）。
	Stop(ctx context.Context) error
}
