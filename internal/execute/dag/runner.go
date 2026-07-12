package dag

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// Runner / Validator 是 execute/dag 对外的消费端适配器（2026-07-12 随
// internal/execute 模块化新增）。
//
// DAGExecutor/ValidateDAG 是本包既有的具体实现，字段与函数签名保持不变（不做
// 顺手重构）。但 dag 包物理迁出 internal/agent 后成为跨模块依赖，internal/agent
// 若继续直接 import 本包会违反 agent/CLAUDE.md 的消费端接口边界（HE-3）。
// Runner/Validator 是两个无状态的薄适配层：其方法签名与 agent/provider.go 中
// DAGRunner/DAGValidator 接口结构一致（Go 结构化接口满足，参数均为 protocol
// 包类型或匿名函数类型，双方均无需相互 import），由 cmd/polaris 组装根构造并
// 注入 Agent（agent.InjectDAGRunner/InjectDAGValidator）。

// Runner 是 DAGExecutor 的无状态工厂适配器：每次 Run 调用内部构造一个一次性
// DAGExecutor（与既有 runExecuteDAG 每次手动 NewDAGExecutor 的用法完全等价）。
type Runner struct{}

// NewRunner 构造 Runner（无依赖，可全局共享同一实例）。
func NewRunner() *Runner { return &Runner{} }

// Run 执行一次完整 DAG 计划，返回节点结果、是否触发降级重规划、错误。
// toolExec/leaseRenew 为匿名函数类型（与 agent.DAGToolExecutorFn/DAGLeaseRenewFn
// 结构一致），避免 Runner 与 agent 包相互 import。
func (r *Runner) Run(
	ctx context.Context,
	plan *DAGPlan,
	toolExec func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error),
	leaseRenew func(ctx context.Context, taskID, agentID string, ttl time.Duration) error,
	taskID, agentID string,
) ([]NodeResult, bool, error) {
	executor := NewDAGExecutor(ToolExecutorFn(toolExec), LeaseRenewFn(leaseRenew))
	results, err := executor.Execute(ctx, plan, taskID, agentID)
	return results, executor.DegradedReplan, err
}

// Validator 是 ValidateDAG 包级函数的无状态适配器。
type Validator struct{}

// NewValidator 构造 Validator（无依赖，可全局共享同一实例）。
func NewValidator() *Validator { return &Validator{} }

// Validate 执行 S_VALIDATE 四层校验管线。
func (v *Validator) Validate(ctx context.Context, vCtx *DAGValidationContext) error {
	return ValidateDAG(ctx, vCtx)
}
