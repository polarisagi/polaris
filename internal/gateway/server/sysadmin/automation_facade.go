package sysadmin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/polarisagi/polaris/internal/automation/pipeline"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/workflowadmin"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// AutomationFacade 汇聚 sysadmin 层提供的自动化管理能力（汇聚 CronAdmin 与 WorkflowAdmin）。
type AutomationFacade struct {
	Cron     *cronadmin.CronAdmin
	Workflow *workflowadmin.WorkflowAdmin
}

// List 返回所有自动化任务的概要（合并 prompt 与 pipeline）。
func (f *AutomationFacade) List(ctx context.Context) ([]protocol.AutomationSummary, error) {
	var results []protocol.AutomationSummary
	if f.Cron != nil {
		res, err := f.Cron.ListAutomationSummaries(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.List cron", err)
		}
		results = append(results, res...)
	}
	if f.Workflow != nil {
		wfRes, err := f.Workflow.ListPipelineSummaries(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.List workflow", err)
		}
		results = append(results, wfRes...)
	}
	return results, nil
}

// SavePrompt 创建或更新 prompt 型自动化任务。
func (f *AutomationFacade) SavePrompt(ctx context.Context, in protocol.PromptAutomationInput) (string, string, error) {
	if f.Cron == nil {
		return "", "", nil
	}
	enabled := in.Enabled
	id, nextRun, err := f.Cron.SaveAutomation(ctx, cronadmin.AutomationInput{
		ID:              in.ID,
		Name:            in.Name,
		Prompt:          in.Prompt,
		TriggerType:     "cron",
		CronSchedule:    in.Cron,
		ReasoningEffort: in.ReasoningEffort,
		ResultAction:    in.ResultAction,
		Enabled:         &enabled,
		Source:          "agent",
	})
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.SavePrompt", err)
	}
	return id, nextRun, nil
}

// SavePipeline 创建或更新 pipeline 型自动化任务。
func (f *AutomationFacade) SavePipeline(ctx context.Context, specJSON []byte, id string) (protocol.PipelineSaveResult, error) {
	if f.Workflow == nil {
		return protocol.PipelineSaveResult{OK: false}, apperr.New(apperr.CodeInternal, "workflow service is not available")
	}
	var spec pipeline.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return protocol.PipelineSaveResult{OK: false}, apperr.Wrap(apperr.CodeInvalidInput, "unmarshal pipeline spec", err)
	}
	wfID, nextRun, errs, err := f.Workflow.SavePipeline(ctx, spec, id, "agent")
	if err != nil {
		return protocol.PipelineSaveResult{OK: false}, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.SavePipeline", err)
	}
	if len(errs) > 0 {
		return protocol.PipelineSaveResult{
			OK:     false,
			Errors: convertPipelineFieldErrors(errs),
		}, nil
	}
	return protocol.PipelineSaveResult{
		OK:        true,
		ID:        wfID,
		NextRunAt: nextRun,
	}, nil
}

// DryRunSpec 对给定的 pipeline Spec JSON 进行同步试跑预览。
func (f *AutomationFacade) DryRunSpec(ctx context.Context, specJSON []byte) (protocol.PipelineDryRunResult, []protocol.FieldError, error) {
	if f.Workflow == nil {
		return protocol.PipelineDryRunResult{}, nil, apperr.New(apperr.CodeInternal, "workflow service is not available")
	}
	var spec pipeline.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return protocol.PipelineDryRunResult{}, nil, apperr.Wrap(apperr.CodeInvalidInput, "unmarshal pipeline spec", err)
	}
	dryRes, errs, err := f.Workflow.DryRunPipeline(ctx, spec)
	if err != nil {
		return protocol.PipelineDryRunResult{}, nil, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.DryRunSpec", err)
	}
	return dryRes, convertPipelineFieldErrors(errs), nil
}

// DryRunByID 对数据库中已存在的管线工作流进行同步试跑预览。
func (f *AutomationFacade) DryRunByID(ctx context.Context, id string) (protocol.PipelineDryRunResult, error) {
	if f.Workflow == nil {
		return protocol.PipelineDryRunResult{}, apperr.New(apperr.CodeInternal, "workflow service is not available")
	}
	res, err := f.Workflow.DryRunWorkflow(ctx, id)
	if err != nil {
		return protocol.PipelineDryRunResult{}, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.DryRunByID", err)
	}
	return res, nil
}

func convertPipelineFieldErrors(errs []pipeline.FieldError) []protocol.FieldError {
	if len(errs) == 0 {
		return nil
	}
	res := make([]protocol.FieldError, len(errs))
	for i, e := range errs {
		res[i] = protocol.FieldError{Path: e.Path, Message: e.Message}
	}
	return res
}

// Delete 删除指定自动化任务（按前缀分派：wf_ 派发给 Workflow，其余派发给 Cron）。
func (f *AutomationFacade) Delete(ctx context.Context, id string) error {
	if strings.HasPrefix(id, "wf_") {
		if f.Workflow != nil {
			if err := f.Workflow.DeleteWorkflow(ctx, id); err != nil {
				return apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.Delete", err)
			}
			return nil
		}
		return nil
	}
	if f.Cron != nil {
		if err := f.Cron.DeleteAutomation(ctx, id); err != nil {
			return apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.Delete", err)
		}
	}
	return nil
}

// RunNow 立即触发指定自动化任务（按前缀分派：wf_ 派发给 Workflow，其余派发给 Cron）。
func (f *AutomationFacade) RunNow(ctx context.Context, id string) (string, error) {
	if strings.HasPrefix(id, "wf_") {
		if f.Workflow != nil {
			runID, err := f.Workflow.TriggerWorkflow(ctx, id)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.RunNow", err)
			}
			return runID, nil
		}
		return "", nil
	}
	if f.Cron != nil {
		runID, err := f.Cron.TriggerAutomation(ctx, id)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.RunNow", err)
		}
		return runID, nil
	}
	return "", nil
}

// CountEnabledBySource 统计指定来源启用的自动化数量（两表之和）。
func (f *AutomationFacade) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	var total int
	if f.Cron != nil {
		count, err := f.Cron.CountEnabledBySource(ctx, source)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.CountEnabledBySource cron", err)
		}
		total += count
	}
	if f.Workflow != nil {
		count, err := f.Workflow.CountEnabledBySource(ctx, source)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.CountEnabledBySource workflow", err)
		}
		total += count
	}
	return total, nil
}
