package governance

// Eval Harness 类型定义。
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §1-4

// EvalCase 评测用例。
type EvalCase struct {
	ID                string
	Name              string
	Description       string
	Severity          int    // P0=0 (阻塞), P1=1 (警告), P2=2 (记录)
	Source            string // manual | synthetic | shadow | incident
	Task              *EvalTask
	ExpectedOutput    string
	ExpectedToolCalls []ExpectedToolCall
	ExpectedState     []byte
	Evaluators        []EvaluatorSpec
	Tags              []string
	CreatedAt         int64
}

// EvalTask 评测任务。
type EvalTask struct {
	Goal        string
	Context     string
	Input       []byte
	Constraints []string
}

// ExpectedToolCall 期望的工具调用。
type ExpectedToolCall struct {
	ToolName string
	Args     map[string]any
	Mode     string // exact | subset | contains
}

// EvaluatorSpec 评测器规格。
type EvaluatorSpec struct {
	Type   string // L1_assertion | L2_schema | L3_trajectory | L4_llm_judge | L5_human
	Config map[string]any
}

// Evaluator 评测器接口（5 层 Pyramid）。
type Evaluator interface {
	Evaluate(trajectory *AgentTrajectory, expected *EvalCase) (*EvalResult, error)
	Type() string
}

// EvalResult 评测结果。
type EvalResult struct {
	Passed        bool
	Scores        map[string]float64
	Details       string
	EvaluatorType string
}

// AgentTrajectory 完整 Agent 执行轨迹。
type AgentTrajectory struct {
	Task   *EvalTask
	Steps  []TrajectoryEvent
	Result *TrajectoryResult
}

// TrajectoryEvent 轨迹事件。
type TrajectoryEvent struct {
	Seq       int
	Timestamp int64
	Type      string // llm_request | llm_response | tool_call | tool_result | state_change
	Data      []byte
}

// TrajectoryResult 轨迹结果。
type TrajectoryResult struct {
	Success    bool
	Output     string
	ToolCalls  []string
	TokensUsed int
	CostUSD    float64
	LatencyMs  int64
}

// EvalRunReport 评测运行报告。
type EvalRunReport struct {
	RunID       string
	TotalCases  int
	PassedCases int
	FailedCases int
	PassRate    float64
	P0PassRate  float64
	P1PassRate  float64
	BlockDeploy bool
	WarnDeploy  bool
}

// DataSplitter 数据集分区（防 M9 过拟合 Holdout Set）。
type DataSplitter struct {
	TrainingSet []*EvalCase
	HoldoutSet  []*EvalCase
}
