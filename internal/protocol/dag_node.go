package protocol

import (
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// DAG 可执行节点跨模块契约（M04 §5.3, M08 §8.2）。
//
// producer: internal/agent/dag（DAGExecutor 具体调度实现，类型别名于此）
// consumer: internal/swarm/planner（TaskDecomposer 分解目标为节点列表）
//
// ExecNode 此前以 internal/agent/dag.ExecNode 具体类型由 internal/swarm/planner
// 直接 import 消费，违反 M04 §B2。现收敛至此，agent/dag 与 swarm/planner 均引用
// 本文件定义，agent/dag 不再是唯一权威源但保留同名别名保证向后兼容。

// NodeStatus 定义节点执行状态。
type NodeStatus int

const (
	NodePending NodeStatus = iota
	NodeRunning
	NodeCompleted
	NodeFailed
	NodeSkipped // 因上游失败而跳过
)

// CompensationAction 描述一个节点失败后的 Saga 逆序补偿动作。
// write_local / write_network 节点必须声明此字段，否则 DAG 校验拒绝。
type CompensationAction struct {
	ToolName   string
	Args       []byte
	TaintLevel types.TaintLevel
}

// ExecNode 是 DAG 中可执行的工具调用节点。
type ExecNode struct {
	ID             string
	ToolName       string
	Args           []byte
	TaintLevel     types.TaintLevel     // 从 Context 继承的污染等级
	DependsOn      []string             // 前驱节点 ID
	Compensation   *CompensationAction  // Saga 补偿动作（有副作用节点必填）
	MaxRetry       int                  // 默认 0（不重试）
	Timeout        time.Duration        // 0 使用全局默认
	Status         NodeStatus           // 节点状态
	IdempotencyKey types.IdempotencyKey // 幂等键
}
