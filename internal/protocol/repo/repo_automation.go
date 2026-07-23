package repo

import "context"

type AutomationRow struct {
	ID              string
	Name            string
	Prompt          string
	TriggerType     string
	CronSchedule    string
	ChannelID       string
	WorkingDir      string
	EnvType         string
	ReasoningEffort string
	ResultAction    string
	SandboxLevel    int
	CedarRulesJSON  string
	Enabled         bool
	RequiresHITL    bool
	RiskLevel       int
	LastRunAt       string
	NextRunAt       string
	RunCount        int
	LastRunStatus   string
	LastRunError    string
	CreatedAt       string
	UpdatedAt       string
	EventFilter     string
}

type AutomationRunRow struct {
	ID             string
	AutomationID   string
	Trigger        string
	Status         string
	SessionID      string
	StartedAt      string
	FinishedAt     string
	ErrorMsg       string
	PromptSnapshot string
}

type AutomationRepository interface {
	CreateAutomation(ctx context.Context, row AutomationRow) error
	UpdateAutomation(ctx context.Context, row AutomationRow) error
	DeleteAutomation(ctx context.Context, id string) error
	GetAutomation(ctx context.Context, id string) (*AutomationRow, error)
	ListAutomations(ctx context.Context) ([]AutomationRow, error)
	ListDueAutomations(ctx context.Context, nowRFC3339 string) ([]AutomationRow, error)
	ListEventAutomations(ctx context.Context) ([]AutomationRow, error)
	// ListWebhookAutomations 返回指定 channelID 上配置为 webhook 触发的 automations
	// （GD-9-001 复核修复：cron_templates_handlers.go TriggerWebhookAutomations
	// 此前直连 SQL，收敛到本方法）。
	ListWebhookAutomations(ctx context.Context, channelID string) ([]AutomationRow, error)

	UpdateAutomationStatus(ctx context.Context, id, lastRunStatus string) error
	UpdateAutomationStatusAndSchedule(ctx context.Context, id, lastRunStatus, lastRunAt, nextRunAt string) error
	UpdateAutomationStats(ctx context.Context, id, status, errMsg, finishedAt string, circuitBreakThreshold int) (int, error)

	CreateRun(ctx context.Context, row AutomationRunRow) error
	UpdateRunStatus(ctx context.Context, id, status, errorMsg, completedAt string, durationMs int64) error
	DeleteRunsByAutomationID(ctx context.Context, automationID string) error
	ListRunsByAutomationID(ctx context.Context, automationID string, limit int) ([]AutomationRunRow, error)
	TimeoutRuns(ctx context.Context, startedBefore string) error
}
