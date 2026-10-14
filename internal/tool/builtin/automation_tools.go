package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cronauto "github.com/polarisagi/polaris/internal/automation"
	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// AutomationService automation_* 工具的消费方接口（HE-3：接口在调用方定义）。
// 由 cmd/polaris 的 automationBridge 晚绑定到 sysadmin 的 CronAdmin 与 WorkflowAdmin。
type AutomationService interface {
	List(ctx context.Context) ([]protocol.AutomationSummary, error)
	SavePrompt(ctx context.Context, in protocol.PromptAutomationInput) (id, nextRun string, err error)
	SavePipeline(ctx context.Context, specJSON []byte, id string) (protocol.PipelineSaveResult, error)
	DryRunSpec(ctx context.Context, specJSON []byte) (protocol.PipelineDryRunResult, []protocol.FieldError, error)
	DryRunByID(ctx context.Context, id string) (protocol.PipelineDryRunResult, error)
	Delete(ctx context.Context, id string) error
	RunNow(ctx context.Context, id string) (runID string, err error)
	CountEnabledBySource(ctx context.Context, source string) (int, error)
}

// ─── automation_list ──────────────────────────────────────────────────────────

// MakeAutomationListFn 返回 automation_list 工具实现。
func MakeAutomationListFn(svc AutomationService) sandbox.InProcessFn {
	return func(ctx context.Context, _ []byte) ([]byte, error) {
		summaries, err := svc.List(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "automation_list: list failed", err)
		}
		if summaries == nil {
			summaries = []protocol.AutomationSummary{}
		}
		res := map[string]any{
			"automations": summaries,
			"count":       len(summaries),
		}
		data, err := json.Marshal(res)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "automation_list: marshal output failed", err)
		}
		return data, nil
	}
}

// ─── automation_save ──────────────────────────────────────────────────────────

type automationSaveInput struct {
	Kind            string            `json:"kind"`
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Prompt          string            `json:"prompt"`
	Cron            string            `json:"cron"`
	Trigger         *pipeline.Trigger `json:"trigger"`
	ReasoningEffort string            `json:"reasoning_effort"`
	ResultAction    string            `json:"result_action"`
	Enabled         *bool             `json:"enabled"`
	DryRun          bool              `json:"dry_run"`
}

func validateSaveKindAndArgs(in *automationSaveInput) error {
	if in.Kind != "prompt" && in.Kind != "pipeline" {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("invalid kind: %s (must be prompt or pipeline)", in.Kind))
	}
	if in.Kind == "prompt" {
		if in.DryRun {
			return apperr.New(apperr.CodeInvalidInput, "prompt 自动化不支持 dry_run 试运行")
		}
		if strings.TrimSpace(in.Prompt) == "" {
			return apperr.New(apperr.CodeInvalidInput, "prompt is required for kind=prompt")
		}
		if strings.TrimSpace(in.Cron) == "" {
			return apperr.New(apperr.CodeInvalidInput, "cron is required for kind=prompt")
		}
	}
	return nil
}

func validateEffortAndActionResult(in *automationSaveInput) error {
	if in.Kind == "prompt" {
		in.ReasoningEffort = strings.ToLower(strings.TrimSpace(in.ReasoningEffort))
		if in.ReasoningEffort == "" {
			in.ReasoningEffort = "low"
		}
		switch in.ReasoningEffort {
		case "low", "medium", "high":
		default:
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("invalid reasoning_effort: %s (must be low, medium, or high)", in.ReasoningEffort))
		}
	}

	in.ResultAction = strings.TrimSpace(in.ResultAction)
	if in.ResultAction == "" {
		in.ResultAction = "session"
	}
	if in.ResultAction != "session" && in.ResultAction != "silent" && !strings.HasPrefix(in.ResultAction, "channel:") {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("invalid result_action: %s (must be session, silent, or channel:<id>)", in.ResultAction))
	}
	if strings.HasPrefix(in.ResultAction, "channel:") && strings.TrimSpace(strings.TrimPrefix(in.ResultAction, "channel:")) == "" {
		return apperr.New(apperr.CodeInvalidInput, "invalid result_action: channel ID is required")
	}
	return nil
}

func checkAgentAutomationQuota(ctx context.Context, svc AutomationService, in *automationSaveInput) error {
	// 配额检查（仅新建且非 dry_run 时生效）
	if in.ID != "" || in.DryRun {
		return nil
	}
	count, err := svc.CountEnabledBySource(ctx, "agent")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "automation_save: count agent automations failed", err)
	}
	maxAgent := config.CurrentThresholds().M13Interface.AgentAutomationMax
	if count >= maxAgent {
		metrics.RecordAutomationAgentWriteDenied(ctx, "limit")
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("automation: Agent 启用自动化数量已达上限 (%d)，请使用 automation_list 查看并删除不再需要的任务", maxAgent))
	}
	return nil
}

func checkPipelineMinInterval(ctx context.Context, in *automationSaveInput) error {
	cronExpr := in.Cron
	triggerType := "manual"
	if in.Trigger != nil {
		if in.Trigger.Cron != "" {
			cronExpr = in.Trigger.Cron
		}
		if in.Trigger.Type != "" {
			triggerType = in.Trigger.Type
		}
	} else if cronExpr != "" {
		triggerType = "cron"
	}

	if triggerType != "cron" || cronExpr == "" {
		return nil
	}

	minIntervalDuration, err := cronauto.MinInterval(cronExpr, time.Now().UTC(), 50)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "automation_save: invalid cron schedule", err)
	}
	minIntervalMinutes := int(minIntervalDuration.Minutes())
	thresholdMin := config.CurrentThresholds().M13Interface.PipelineMinIntervalMin
	if minIntervalMinutes < thresholdMin {
		metrics.RecordAutomationAgentWriteDenied(ctx, "interval")
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("automation: pipeline 自动化最小调度间隔为 %d 分钟，当前表达式最小间隔为 %d 分钟", thresholdMin, minIntervalMinutes))
	}
	return nil
}

func checkPromptMinInterval(ctx context.Context, in *automationSaveInput) error {
	minIntervalDuration, err := cronauto.MinInterval(in.Cron, time.Now().UTC(), 50)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "automation_save: invalid cron schedule", err)
	}
	minIntervalMinutes := int(minIntervalDuration.Minutes())
	thresholdMin := config.CurrentThresholds().M13Interface.PromptAutomationMinIntervalMin
	if minIntervalMinutes < thresholdMin {
		metrics.RecordAutomationAgentWriteDenied(ctx, "interval")
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("automation: prompt 自动化最小调度间隔为 %d 分钟，当前表达式最小间隔为 %d 分钟", thresholdMin, minIntervalMinutes))
	}
	return nil
}

func checkSaveGuardrails(ctx context.Context, svc AutomationService, in *automationSaveInput) error {
	if err := checkAgentAutomationQuota(ctx, svc, in); err != nil {
		return err
	}
	if in.Kind == "pipeline" {
		return checkPipelineMinInterval(ctx, in)
	}
	return checkPromptMinInterval(ctx, in)
}

func handleDryRunSave(ctx context.Context, svc AutomationService, input []byte) ([]byte, error) {
	dryRes, fieldErrs, err := svc.DryRunSpec(ctx, input)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "automation_save: dry run failed", err)
	}
	res := map[string]any{
		"ok":      len(fieldErrs) == 0,
		"dry_run": dryRes,
		"errors":  fieldErrs,
	}
	if fieldErrs == nil {
		res["errors"] = []protocol.FieldError{}
	}
	data, err := json.Marshal(res)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "automation_save: marshal output failed", err)
	}
	return data, nil
}

func handlePipelineSave(ctx context.Context, svc AutomationService, input []byte, in *automationSaveInput) ([]byte, error) {
	saveRes, err := svc.SavePipeline(ctx, input, in.ID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "automation_save: save pipeline failed", err)
	}
	res := map[string]any{
		"ok":          saveRes.OK,
		"id":          saveRes.ID,
		"next_run_at": saveRes.NextRunAt,
		"errors":      saveRes.Errors,
	}
	if saveRes.Errors == nil {
		res["errors"] = []protocol.FieldError{}
	}
	if saveRes.OK {
		res["status"] = "saved"
	}
	data, err := json.Marshal(res)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "automation_save: marshal output failed", err)
	}
	return data, nil
}

func handlePromptSave(ctx context.Context, svc AutomationService, in *automationSaveInput) ([]byte, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	id, nextRun, err := svc.SavePrompt(ctx, protocol.PromptAutomationInput{
		ID:              in.ID,
		Name:            in.Name,
		Prompt:          in.Prompt,
		Cron:            in.Cron,
		ReasoningEffort: in.ReasoningEffort,
		ResultAction:    in.ResultAction,
		Enabled:         enabled,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "automation_save: save prompt failed", err)
	}

	res := map[string]any{
		"ok":          true,
		"id":          id,
		"next_run_at": nextRun,
		"status":      "saved",
	}
	data, err := json.Marshal(res)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "automation_save: marshal output failed", err)
	}
	return data, nil
}

// MakeAutomationSaveFn 返回 automation_save 工具实现。
func MakeAutomationSaveFn(svc AutomationService) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if protocol.IsBackgroundWork(ctx) {
			metrics.RecordAutomationAgentWriteDenied(ctx, "headless_origin")
			return nil, apperr.New(apperr.CodeForbidden, "automation: 无头后台任务禁止创建/修改自动化")
		}

		var in automationSaveInput
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "automation_save: invalid input JSON", err)
		}

		if err := validateSaveKindAndArgs(&in); err != nil {
			return nil, err
		}
		if err := validateEffortAndActionResult(&in); err != nil {
			return nil, err
		}
		if err := checkSaveGuardrails(ctx, svc, &in); err != nil {
			return nil, err
		}

		if in.DryRun {
			return handleDryRunSave(ctx, svc, input)
		}
		if in.Kind == "pipeline" {
			return handlePipelineSave(ctx, svc, input, &in)
		}
		return handlePromptSave(ctx, svc, &in)
	}
}

// ─── automation_run_now ───────────────────────────────────────────────────────

// MakeAutomationRunNowFn 返回 automation_run_now 工具实现。
func MakeAutomationRunNowFn(svc AutomationService) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if protocol.IsBackgroundWork(ctx) {
			metrics.RecordAutomationAgentWriteDenied(ctx, "headless_origin")
			return nil, apperr.New(apperr.CodeForbidden, "automation: 无头后台任务禁止触发自动化")
		}

		var in struct {
			ID     string `json:"id"`
			DryRun bool   `json:"dry_run"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "automation_run_now: invalid input JSON", err)
		}
		if strings.TrimSpace(in.ID) == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "id is required")
		}

		if in.DryRun {
			if !strings.HasPrefix(in.ID, "wf_") {
				return nil, apperr.New(apperr.CodeInvalidInput, "prompt 自动化不支持 dry_run 试运行")
			}
			dryRes, err := svc.DryRunByID(ctx, in.ID)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeOf(err), "automation_run_now: dry run by ID failed", err)
			}
			res := map[string]any{
				"ok":      true,
				"dry_run": dryRes,
			}
			data, err := json.Marshal(res)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "automation_run_now: marshal output failed", err)
			}
			return data, nil
		}

		runID, err := svc.RunNow(ctx, in.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "automation_run_now: run failed", err)
		}

		res := map[string]any{
			"id":     in.ID,
			"run_id": runID,
			"status": "triggered",
		}
		data, err := json.Marshal(res)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "automation_run_now: marshal output failed", err)
		}
		return data, nil
	}
}

// ─── automation_delete ────────────────────────────────────────────────────────

// MakeAutomationDeleteFn 返回 automation_delete 工具实现。
func MakeAutomationDeleteFn(svc AutomationService) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if protocol.IsBackgroundWork(ctx) {
			metrics.RecordAutomationAgentWriteDenied(ctx, "headless_origin")
			return nil, apperr.New(apperr.CodeForbidden, "automation: 无头后台任务禁止删除自动化")
		}

		var in struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "automation_delete: invalid input JSON", err)
		}
		if strings.TrimSpace(in.ID) == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "id is required")
		}

		if err := svc.Delete(ctx, in.ID); err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "automation_delete: delete failed", err)
		}

		res := map[string]any{
			"id":     in.ID,
			"status": "deleted",
		}
		data, err := json.Marshal(res)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "automation_delete: marshal output failed", err)
		}
		return data, nil
	}
}
