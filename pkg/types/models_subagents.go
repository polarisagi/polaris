package types

// SpawnTaskSpec 定义单个子智能体派生任务的配置（ADR-0115 WP4）。
type SpawnTaskSpec struct {
	Prompt         string   `json:"prompt"`
	Role           string   `json:"role,omitempty"`
	Model          string   `json:"model,omitempty"`
	Thinking       string   `json:"thinking,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	ReadOnly       bool     `json:"read_only,omitempty"`
	MaxResultChars int      `json:"max_result_chars,omitempty"` // 默认 4000，超出卸载为 read_tool_ref
}

// SpawnAgentsArgs spawn_agents 工具入参。
type SpawnAgentsArgs struct {
	Tasks []SpawnTaskSpec `json:"tasks"`
	Wait  string          `json:"wait,omitempty"` // "all" | "none"，默认 "all"
}

// SubagentStatusInfo 描述子智能体当前状态摘要。
type SubagentStatusInfo struct {
	ID         string `json:"id"`
	Status     string `json:"status"` // pending | running | done | failed | cancelled
	Role       string `json:"role,omitempty"`
	Model      string `json:"model,omitempty"`
	Thinking   string `json:"thinking,omitempty"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// AgentStatusArgs agent_status 工具入参。
type AgentStatusArgs struct {
	IDs []string `json:"ids,omitempty"`
}

// AgentResultArgs agent_result 工具入参。
type AgentResultArgs struct {
	ID string `json:"id"`
}

// AgentMessageArgs agent_message 工具入参（向子智能体插话）。
type AgentMessageArgs struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// AgentStopArgs agent_stop 工具入参（中止子智能体）。
type AgentStopArgs struct {
	ID string `json:"id"`
}
