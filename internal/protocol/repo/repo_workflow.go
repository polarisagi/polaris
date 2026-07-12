package repo

import "context"

type WorkflowStepRow struct {
	ID              string
	WorkflowID      string
	Seq             int
	Name            string
	AutomationID    string
	Prompt          string
	ReasoningEffort string
	WorkingDir      string
	InputFromPrev   bool
	// DependsOn 非空时表达真实 DAG 依赖（前驱 step ID 列表），由 workflowadmin 的
	// buildGraphSpec 构造 StateGraphExecutor 无条件边；为空时按 Seq 合成顺序链
	// （向后兼容既有 chain 语义）。JSON 序列化形态见 029_workflows.sql depends_on 列。
	DependsOn        []string
	CapabilityType   string
	CompensationTool string
	CompensationArgs string
	// MaxRetries>0 时 buildGraphSpec 为该节点附加自环条件边（Field=status,Op=eq,
	// Value=error）+ MaxVisits=1+MaxRetries；与 CompensationTool 非空互斥（Saga
	// 逆序补偿语义在多次执行节点上未定义，StateGraphExecutor 校验阶段拒绝）。
	MaxRetries int
}

type WorkflowRow struct {
	ID           string
	Name         string
	Description  string
	TriggerType  string
	CronSchedule string
	Enabled      bool
	NextRunAt    string
	CreatedAt    string
	UpdatedAt    string
	// Type 'chain'（默认，忽略 WorkflowStepRow.DependsOn，按 Seq 合成顺序链，完全
	// 向后兼容既有语义）| 'dag'（如实按 DependsOn 构造并行边，支持多前驱 AND-Join，
	// 空 DependsOn 视为真实并行入口）。决定 workflow_graph.go buildGraphSpec 的分支。
	Type string
}

type WorkflowRepository interface {
	CreateWorkflowWithSteps(ctx context.Context, wf WorkflowRow, steps []WorkflowStepRow) error
	UpdateWorkflowWithSteps(ctx context.Context, wf WorkflowRow, steps []WorkflowStepRow, updateSteps bool) error
	DeleteWorkflow(ctx context.Context, wfID string) error

	CreateWorkflowRun(ctx context.Context, runID, wfID, trigger, status string, currentStep, totalSteps int, startedAt string) error
	UpdateWorkflowRunStatus(ctx context.Context, runID, status, finishedAt, errMsg, stepOutputs string, currentStep int) error
	UpdateWorkflowRunCurrentStep(ctx context.Context, runID string, currentStep int) error
	UpdateWorkflowLastRun(ctx context.Context, wfID, lastRunAt, nextRunAt, lastRunStatus, updatedAt string) error
	UpdateWorkflowStats(ctx context.Context, wfID, status, errMsg, finishedAt string, circuitBreakThreshold int) error

	// AppendWorkflowRunStepOutput 原子追加单条步骤输出到 workflow_runs.step_outputs
	// JSON 数组（SQLite json_insert，单条 UPDATE 语句内完成读改写，避免 DAG 并行
	// 执行下多个步骤并发完成时的"读-改-写"竞态丢失更新）。stepOutputJSON 为单个
	// stepOutput 对象的 JSON 编码。
	AppendWorkflowRunStepOutput(ctx context.Context, runID string, stepOutputJSON []byte) error
	// IncrementWorkflowRunCurrentStep 原子自增 current_step（DAG 并行模式下代表
	// "已完成步骤数"而非某个具体的顺序位置，语义随 buildGraphSpec 接入而调整，
	// 见 workflow_engine.go executeWorkflow 注释）。
	IncrementWorkflowRunCurrentStep(ctx context.Context, runID string) error
}
