// 数据模型 + ID 生成器。HTTP handler 见 workflow_handlers.go，执行引擎见
// workflow_engine.go，cron 触发 + 辅助函数见 workflow_cron.go
// （2026-07-07 R7 瘦身：原 730 行单文件按职责拆分，均 ≤400 行）。
package workflowadmin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

type workflow struct {
	ID string `json:"id"`
	// Type 'chain'（默认）| 'dag'，见 repo.WorkflowRow.Type 注释。
	Type           string  `json:"type"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	TriggerType    string  `json:"trigger_type"`
	CronSchedule   string  `json:"cron_schedule"`
	Enabled        bool    `json:"enabled"`
	StepsCount     int     `json:"steps_count"`
	LastRunAt      string  `json:"last_run_at"`
	NextRunAt      string  `json:"next_run_at"`
	RunCount       int     `json:"run_count"`
	LastRunStatus  string  `json:"last_run_status"`
	LastRunError   string  `json:"last_run_error"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	ResultAction   string  `json:"result_action,omitempty"`
	DeliverFrom    string  `json:"deliver_from,omitempty"`
	Source         string  `json:"source,omitempty"`
	LastRunSkipped bool    `json:"last_run_skipped,omitempty"`
	LastRunTokens  int     `json:"last_run_tokens,omitempty"`
	LastRunCost    float64 `json:"last_run_cost,omitempty"`
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

	// DependsOn 为空时按 Seq 合成顺序链（向后兼容）；非空时表达真实 DAG 依赖，
	// 由 buildGraphSpec（workflow_graph.go）转换为 StateGraphExecutor 的无条件边。
	DependsOn        []string `json:"depends_on"`
	CapabilityType   string   `json:"capability_type"`
	CompensationTool string   `json:"compensation_tool"`
	CompensationArgs string   `json:"compensation_args"`
	// MaxRetries>0 时附加失败重试自环边；与 CompensationTool 非空互斥。
	MaxRetries int `json:"max_retries"`

	// ADR-0114: 管线步骤专用属性
	StepKey    string `json:"step_key,omitempty"`
	Kind       string `json:"kind,omitempty"`
	ConfigJSON string `json:"config_json,omitempty"`
}

func (s *workflowStep) UnmarshalJSON(data []byte) error {
	type Alias workflowStep
	aux := struct {
		*Alias
		ConfigRaw json.RawMessage `json:"config_json"`
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "unmarshal workflow step", err)
	}
	if len(aux.ConfigRaw) > 0 {
		s.ConfigJSON = normalizeConfigJSON(aux.ConfigRaw)
	}
	return nil
}

func normalizeConfigJSON(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "{}"
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(raw, &unquoted); err == nil {
			return unquoted
		}
	}
	return s
}

type workflowRun struct {
	ID           string          `json:"id"`
	WorkflowID   string          `json:"workflow_id"`
	Trigger      string          `json:"trigger"`
	Status       string          `json:"status"`
	CurrentStep  int             `json:"current_step"`
	TotalSteps   int             `json:"total_steps"`
	StartedAt    string          `json:"started_at"`
	FinishedAt   string          `json:"finished_at"`
	ErrorMsg     string          `json:"error_msg"`
	StepOutputs  json.RawMessage `json:"step_outputs"`
	Skipped      int             `json:"skipped"`
	LLMTokensIn  int64           `json:"llm_tokens_in"`
	LLMTokensOut int64           `json:"llm_tokens_out"`
	CostUSD      float64         `json:"cost_usd"`
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
