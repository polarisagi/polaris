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
	ReasoningEffort string
	ResultAction    string
	SandboxLevel    int
	CedarRulesJSON  string
	Enabled         bool
	RequiresHITL    bool
	RiskLevel       string
	NextRunAt       string
	LastRunStatus   string
	CreatedAt       string
	UpdatedAt       string
	EventFilter     string
}

type AutomationRunRow struct {
	ID           string
	AutomationID string
	Status       string
	Error        string
	StartedAt    string
	CompletedAt  string
	DurationMs   int64
	LogFile      string
	ArtifactURI  string
}

type AutomationRepository interface {
	CreateAutomation(ctx context.Context, row AutomationRow) error
	UpdateAutomation(ctx context.Context, row AutomationRow) error
	DeleteAutomation(ctx context.Context, id string) error
	GetAutomation(ctx context.Context, id string) (*AutomationRow, error)
	ListAutomations(ctx context.Context) ([]AutomationRow, error)

	UpdateAutomationStatus(ctx context.Context, id, lastRunStatus string) error
	UpdateAutomationStatusAndSchedule(ctx context.Context, id, lastRunStatus, lastRunAt, nextRunAt string) error
	UpdateAutomationStats(ctx context.Context, id, status, errMsg, finishedAt string, circuitBreakThreshold int) (int, error)

	CreateRun(ctx context.Context, row AutomationRunRow) error
	UpdateRunStatus(ctx context.Context, id, status, errorMsg, completedAt string, durationMs int64) error
	DeleteRunsByAutomationID(ctx context.Context, automationID string) error
	TimeoutRuns(ctx context.Context, startedBefore string) error
}
