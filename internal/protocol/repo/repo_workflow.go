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
}
