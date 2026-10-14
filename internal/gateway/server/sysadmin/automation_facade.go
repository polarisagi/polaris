package sysadmin

import (
	"context"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// AutomationFacade 汇聚 sysadmin 层提供的自动化管理能力（当前为 CronAdmin，T6 追加 WorkflowAdmin）。
type AutomationFacade struct {
	Cron *cronadmin.CronAdmin
}

// List 返回所有自动化任务的概要。
func (f *AutomationFacade) List(ctx context.Context) ([]protocol.AutomationSummary, error) {
	if f.Cron == nil {
		return nil, nil
	}
	res, err := f.Cron.ListAutomationSummaries(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.List", err)
	}
	return res, nil
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

// Delete 删除指定自动化任务。
func (f *AutomationFacade) Delete(ctx context.Context, id string) error {
	if f.Cron == nil {
		return nil
	}
	if err := f.Cron.DeleteAutomation(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.Delete", err)
	}
	return nil
}

// RunNow 立即触发指定自动化任务。
func (f *AutomationFacade) RunNow(ctx context.Context, id string) (string, error) {
	if f.Cron == nil {
		return "", nil
	}
	runID, err := f.Cron.TriggerAutomation(ctx, id)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.RunNow", err)
	}
	return runID, nil
}

// CountEnabledBySource 统计指定来源启用的自动化数量。
func (f *AutomationFacade) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	if f.Cron == nil {
		return 0, nil
	}
	count, err := f.Cron.CountEnabledBySource(ctx, source)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeOf(err), "AutomationFacade.CountEnabledBySource", err)
	}
	return count, nil
}
