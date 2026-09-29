package repo

// session_trajectory（045）的 event_type 取值。状态迁移事件的 event_type 是状态编号字符串，不在此枚举。
const (
	TrajectoryEventLLMCall  = "llm_call"
	TrajectoryEventToolCall = "tool_call"
)

// TrajectoryRow session_trajectory 一行。ToolOK 为 nil 表示非工具事件（NULL）；
// LatencyMs 为 -1 表示未记录（NULL），工具事件 0 是合法耗时。
type TrajectoryRow struct {
	ID          int64
	SessionID   string
	Seq         int64
	EventType   string
	ToolName    string
	ToolOK      *bool
	LatencyMs   int64
	Payload     string
	CreatedAtMs int64
}
