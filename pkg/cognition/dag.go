// Deprecated: DAG 数据模型已迁移到 pkg/cognition/kernel/dag_executor.go。
// 此文件保留以兼容旧调用方，新代码应直接使用 kernel.ExecNode / kernel.ExecEdge / kernel.DAGExecutor。
package cognition

import (
	"context"
	"sync"
	"time"
)

// DAG 数据模型与执行器。
// 架构文档: docs/arch/04-Agent-Kernel-深度选型.md §5

type DAGNode struct {
	ID           string        `json:"id"`
	Type         NodeType      `json:"type"`
	ToolName     string        `json:"tool_name"`
	Input        []byte        `json:"input"`
	DependsOn    []string      `json:"depends_on"`
	RetryPolicy  RetryPolicy   `json:"retry_policy"`
	TimeoutMs    int           `json:"timeout_ms"`
	RiskLevel    int           `json:"risk_level"`
	Compensation *Compensation `json:"compensation,omitempty"`
	Status       NodeStatus    `json:"status"`
}

type NodeType string

const (
	NodeToolCall    NodeType = "tool_call"
	NodeLLMFill     NodeType = "llm_fill"
	NodeParallel    NodeType = "parallel"
	NodeConditional NodeType = "conditional"
	NodeSubDAG      NodeType = "sub_dag"
)

type NodeStatus int

const (
	NodePending NodeStatus = iota
	NodeExecuting
	NodeSucceeded
	NodeFailed
	NodeCompensating
	NodeCompensated
)

type DAGEdge struct {
	From     string       `json:"from"`
	To       string       `json:"to"`
	Polarity EdgePolarity `json:"polarity"`
	Weight   float64      `json:"weight"`
}

type EdgePolarity int

const (
	EdgePositive EdgePolarity = iota
	EdgeNegative
	EdgePrecondition
	EdgeSequence
	EdgeSubsumes
)

type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

type Compensation struct {
	ToolName string
	Input    []byte
}

// DAGExecutor 并发执行 DAG 节点（Deprecated: 请使用 kernel.DAGExecutor）。
type DAGExecutor struct {
	maxConcurrent int
	nodes         []DAGNode
	edges         []DAGEdge
	completed     map[string]bool
	results       map[string][]byte
	mu            sync.Mutex
}

// NodeRunner 节点执行器接口（Deprecated: 请使用 kernel.DAGExecutor）。
type NodeRunner interface {
	Run(ctx context.Context, node DAGNode) ([]byte, error)
	Undo(ctx context.Context, nodeID string) error
}
