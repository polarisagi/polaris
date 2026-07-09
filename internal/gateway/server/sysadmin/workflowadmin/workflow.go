// 数据模型 + ID 生成器。HTTP handler 见 workflow_handlers.go，执行引擎见
// workflow_engine.go，cron 触发 + 辅助函数见 workflow_cron.go
// （2026-07-07 R7 瘦身：原 730 行单文件按职责拆分，均 ≤400 行）。
package workflowadmin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

type workflow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	TriggerType   string `json:"trigger_type"`
	CronSchedule  string `json:"cron_schedule"`
	Enabled       bool   `json:"enabled"`
	StepsCount    int    `json:"steps_count"`
	LastRunAt     string `json:"last_run_at"`
	NextRunAt     string `json:"next_run_at"`
	RunCount      int    `json:"run_count"`
	LastRunStatus string `json:"last_run_status"`
	LastRunError  string `json:"last_run_error"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type workflowStep struct {
	ID              string `json:"id"`
	WorkflowID      string `json:"workflow_id"`
	Seq             int    `json:"seq"`
	Name            string `json:"name"`
	AutomationID    string `json:"automation_id"`
	Prompt          string `json:"prompt"`
	ReasoningEffort string `json:"reasoning_effort"`
	WorkingDir      string `json:"working_dir"`
	InputFromPrev   bool   `json:"input_from_prev"`
}

type workflowRun struct {
	ID          string          `json:"id"`
	WorkflowID  string          `json:"workflow_id"`
	Trigger     string          `json:"trigger"`
	Status      string          `json:"status"`
	CurrentStep int             `json:"current_step"`
	TotalSteps  int             `json:"total_steps"`
	StartedAt   string          `json:"started_at"`
	FinishedAt  string          `json:"finished_at"`
	ErrorMsg    string          `json:"error_msg"`
	StepOutputs json.RawMessage `json:"step_outputs"`
}

type stepOutput struct {
	Seq           int    `json:"seq"`
	SessionID     string `json:"session_id"`
	Status        string `json:"status"`
	OutputPreview string `json:"output_preview"`
}

func newWorkflowID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "wf_" + hex.EncodeToString(b)
}

func newWorkflowStepID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "ws_" + hex.EncodeToString(b)
}

func newWorkflowRunID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "wfr_" + hex.EncodeToString(b)
}
