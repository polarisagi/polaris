package cronadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	cronauto "github.com/polarisagi/polaris/internal/automation"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// AutomationInput 创建/更新 prompt 型自动化的规范化输入；HTTP 与 automation_save 工具共用。
type AutomationInput struct {
	ID              string `json:"id"` // 空=创建
	Name            string `json:"name"`
	Prompt          string `json:"prompt"`
	TriggerType     string `json:"trigger_type"` // 默认 cron
	CronSchedule    string `json:"cron_schedule"`
	ChannelID       string `json:"channel_id"`
	WorkingDir      string `json:"working_dir"`
	EnvType         string `json:"env_type"`
	ReasoningEffort string `json:"reasoning_effort"`
	ResultAction    string `json:"result_action"`
	CedarRulesJSON  string `json:"cedar_rules_json"`
	EventFilter     string `json:"event_filter"`
	Enabled         *bool  `json:"enabled"`
	RequiresHITL    bool   `json:"requires_hitl"`
	RiskLevel       int    `json:"risk_level"`
	Source          string `json:"source"` // 仅创建时生效
	// ProjectID 自动化所属项目。创建时空 = 创建者所在项目（Agent 工具）或默认项目；更新时空 = 保持原项目。
	ProjectID string `json:"project_id"`
}

// MinIntervalMinutes 连续计算 50 次触发时间，取相邻差最小值（以分钟为单位）。
func MinIntervalMinutes(expr string, from time.Time) (int, error) {
	d, err := cronauto.MinInterval(expr, from, 50)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "cronadmin: MinInterval", err)
	}
	return int(d.Minutes()), nil
}

func validateTriggerAndCron(in *AutomationInput) (string, error) {
	if in.TriggerType == "" {
		in.TriggerType = "cron"
	}
	switch in.TriggerType {
	case "cron", "webhook", "both", "manual", "event":
	default:
		return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("invalid trigger_type: %s", in.TriggerType))
	}

	if (in.TriggerType == "cron" || in.TriggerType == "both") && strings.TrimSpace(in.CronSchedule) == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "cron_schedule is required for trigger_type=cron/both")
	}

	if in.CronSchedule == "" {
		return "", nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	nextRun := CalcNextRun(in.CronSchedule, now)
	if nextRun == "" {
		return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("invalid cron_schedule: %s", in.CronSchedule))
	}
	return nextRun, nil
}

func validateEffortAndAction(in *AutomationInput) error {
	if in.ReasoningEffort == "" {
		in.ReasoningEffort = "medium"
	}
	switch in.ReasoningEffort {
	case "low", "medium", "high", "ultra":
	default:
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("invalid reasoning_effort: %s", in.ReasoningEffort))
	}

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

// validateAndNormalizeAutomationInput 校验公共字段并在合法时补全默认值与 nextRun 计算。
func validateAndNormalizeAutomationInput(in *AutomationInput) (string, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "prompt is required")
	}
	nextRun, err := validateTriggerAndCron(in)
	if err != nil {
		return "", err
	}
	if err := validateEffortAndAction(in); err != nil {
		return "", err
	}
	if in.CedarRulesJSON == "" {
		in.CedarRulesJSON = "[]"
	}
	if in.EventFilter == "" {
		in.EventFilter = "{}"
	}
	if in.EnvType == "" {
		in.EnvType = "inline"
	}
	if in.RiskLevel <= 0 {
		in.RiskLevel = 1
	}
	return nextRun, nil
}

// resolveProject 校验并确定自动化所属项目；返回空串表示“沿用默认/原值”由调用方决定。
// 项目必须存在且未归档：归档项目不接收新会话（ADR-0097），自动化每次运行都会新建会话。
func (ca *CronAdmin) resolveProject(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || ca.DB == nil {
		return requested, nil
	}
	var archived int
	err := ca.DB.QueryRowContext(ctx, `SELECT archived FROM projects WHERE id=?`, requested).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("project %s not found", requested))
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "cronadmin: check project failed", err)
	}
	if archived == 1 {
		return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("project %s is archived", requested))
	}
	return requested, nil
}

// creatorProject Agent 工具创建自动化时，默认归入创建者当前会话所在的项目（“创建时所在项目”）。
func (ca *CronAdmin) creatorProject(ctx context.Context) string {
	sid, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	if sid == "" || ca.DB == nil {
		return ""
	}
	var pid string
	if err := ca.DB.QueryRowContext(ctx, `SELECT project_id FROM chat_sessions WHERE id=?`, sid).Scan(&pid); err != nil {
		return ""
	}
	return pid
}

func (ca *CronAdmin) createAutomation(ctx context.Context, in AutomationInput, nextRun string) (string, string, error) {
	projectID := in.ProjectID
	if projectID == "" {
		projectID = ca.creatorProject(ctx)
	}
	projectID, err := ca.resolveProject(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	id := newAutoID()
	source := in.Source
	if source == "" {
		source = "user"
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := time.Now().UTC().Format(time.RFC3339)
	row := repo.AutomationRow{
		ID:              id,
		Name:            in.Name,
		Prompt:          in.Prompt,
		TriggerType:     in.TriggerType,
		CronSchedule:    in.CronSchedule,
		ChannelID:       in.ChannelID,
		WorkingDir:      in.WorkingDir,
		EnvType:         in.EnvType,
		ReasoningEffort: in.ReasoningEffort,
		ResultAction:    in.ResultAction,
		SandboxLevel:    2,
		CedarRulesJSON:  in.CedarRulesJSON,
		Enabled:         enabled,
		RequiresHITL:    in.RequiresHITL,
		RiskLevel:       in.RiskLevel,
		Source:          source,
		NextRunAt:       nextRun,
		CreatedAt:       now,
		UpdatedAt:       now,
		EventFilter:     in.EventFilter,
		ProjectID:       projectID,
	}
	if err := ca.AutomationRepo.CreateAutomation(ctx, row); err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "cronadmin: create automation failed", err)
	}
	return id, nextRun, nil
}

func (ca *CronAdmin) updateAutomation(ctx context.Context, in AutomationInput, nextRun string) (string, string, error) {
	existing, err := ca.AutomationRepo.GetAutomation(ctx, in.ID)
	if err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "cronadmin: get automation failed", err)
	}
	if existing == nil {
		return "", "", apperr.New(apperr.CodeNotFound, fmt.Sprintf("automation %s not found", in.ID))
	}
	// 项目：未指定保持原值；改项目要校验目标存在且未归档（原项目已归档但没改它时不阻断编辑）。
	projectID := existing.ProjectID
	if in.ProjectID != "" && in.ProjectID != existing.ProjectID {
		if projectID, err = ca.resolveProject(ctx, in.ProjectID); err != nil {
			return "", "", err
		}
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := time.Now().UTC().Format(time.RFC3339)
	row := repo.AutomationRow{
		ID:              in.ID,
		Name:            in.Name,
		Prompt:          in.Prompt,
		TriggerType:     in.TriggerType,
		CronSchedule:    in.CronSchedule,
		ChannelID:       in.ChannelID,
		WorkingDir:      in.WorkingDir,
		EnvType:         in.EnvType,
		ReasoningEffort: in.ReasoningEffort,
		ResultAction:    in.ResultAction,
		SandboxLevel:    2,
		CedarRulesJSON:  in.CedarRulesJSON,
		Enabled:         enabled,
		RequiresHITL:    in.RequiresHITL,
		RiskLevel:       in.RiskLevel,
		Source:          existing.Source, // 保持创建时来源
		NextRunAt:       nextRun,
		UpdatedAt:       now,
		EventFilter:     in.EventFilter,
		ProjectID:       projectID,
	}
	if err := ca.AutomationRepo.UpdateAutomation(ctx, row); err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "cronadmin: update automation failed", err)
	}
	return in.ID, nextRun, nil
}

// SaveAutomation 统一校验输入、填补默认值、计算 next_run_at 并落库。
func (ca *CronAdmin) SaveAutomation(ctx context.Context, in AutomationInput) (id, nextRun string, err error) {
	nextRun, err = validateAndNormalizeAutomationInput(&in)
	if err != nil {
		return "", "", err
	}
	if in.ID == "" {
		return ca.createAutomation(ctx, in, nextRun)
	}
	return ca.updateAutomation(ctx, in, nextRun)
}

// DeleteAutomation 删除自动化任务及其历史执行记录。
func (ca *CronAdmin) DeleteAutomation(ctx context.Context, id string) error {
	existing, err := ca.AutomationRepo.GetAutomation(ctx, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cronadmin: get automation failed", err)
	}
	if existing == nil {
		return apperr.New(apperr.CodeNotFound, fmt.Sprintf("automation %s not found", id))
	}
	if err := ca.AutomationRepo.DeleteRunsByAutomationID(ctx, id); err != nil {
		slog.Warn("cronadmin: delete runs failed", "id", id, "err", err)
	}
	if err := ca.AutomationRepo.DeleteAutomation(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cronadmin: delete automation failed", err)
	}
	return nil
}

// TriggerAutomation 手动立即触发指定自动化任务执行。
func (ca *CronAdmin) TriggerAutomation(ctx context.Context, id string) (runID string, err error) {
	row, err := ca.AutomationRepo.GetAutomation(ctx, id)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "cronadmin: get automation failed", err)
	}
	if row == nil {
		return "", apperr.New(apperr.CodeNotFound, fmt.Sprintf("automation %s not found", id))
	}
	if !row.Enabled {
		return "", apperr.New(apperr.CodeConflict, "automation is disabled")
	}

	a := automationFromRow(*row)
	runID = ca.executeAutomation(ctx, &a, "manual")
	return runID, nil
}

// ListAutomationSummaries 返回所有自动化任务的摘要列表。
func (ca *CronAdmin) ListAutomationSummaries(ctx context.Context) ([]protocol.AutomationSummary, error) {
	rows, err := ca.AutomationRepo.ListAutomations(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cronadmin: list automations failed", err)
	}
	summaries := make([]protocol.AutomationSummary, len(rows))
	for i, r := range rows {
		summaries[i] = protocol.AutomationSummary{
			ID:            r.ID,
			Kind:          "prompt",
			Name:          r.Name,
			TriggerType:   r.TriggerType,
			Cron:          r.CronSchedule,
			Source:        r.Source,
			LastRunStatus: r.LastRunStatus,
			LastRunAt:     r.LastRunAt,
			NextRunAt:     r.NextRunAt,
			Enabled:       r.Enabled,
			CircuitOpen:   r.CircuitOpen,
		}
	}
	return summaries, nil
}

// CountEnabledBySource 统计指定来源且当前处于启用状态的自动化数量。
func (ca *CronAdmin) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	if ca.AutomationRepo == nil {
		return 0, nil
	}
	count, err := ca.AutomationRepo.CountEnabledBySource(ctx, source)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "cronadmin: count enabled by source failed", err)
	}
	return count, nil
}
