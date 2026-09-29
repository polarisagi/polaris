package repo

// SubagentRunRow 子 Agent 运行记录起始行（042_subagent_runs.sql）。
// Entry ∈ {delegation, fork_skill, hook}；task_id 仅委派入口非空。
type SubagentRunRow struct {
	ID              string
	ParentSessionID string
	ChildSessionID  string
	TaskID          string
	AgentType       string
	Entry           string
	Prompt          string
	StartedAtMs     int64
}
